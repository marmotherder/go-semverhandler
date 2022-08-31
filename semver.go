package semverhandler

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/blang/semver"
	"github.com/leodido/go-conventionalcommits"
	"github.com/leodido/go-conventionalcommits/parser"
	"github.com/marmotherder/go-gitcliwrapper"
)

type Version struct {
	VersionString string
	Version       semver.Version
	ReleaseCommit string
	ReleaseString string
}

const (
	majorIncrement = 4
	minorIncrement = 3
	patchIncrement = 2
	buildIncrement = 1
)

type logger interface {
	Debug(args ...any)
	Debugf(template string, args ...any)
	Info(args ...any)
	Infof(template string, args ...any)
	Warn(args ...any)
	Warnf(template string, args ...any)
	Error(args ...any)
	Errorf(template string, args ...any)
}

type SemverHandler struct {
	Logger        logger
	Git           *gitcliwrapper.GitCLIWrapper
	ReleasePrefix *string
}

func (s SemverHandler) getBranchPrefix() string {
	if s.ReleasePrefix != nil {
		return *s.ReleasePrefix
	}
	return ""
}

func (s SemverHandler) getLastCommitOnRef(ref string, useTags bool) (*string, error) {
	fullRef := s.getBranchPrefix() + "/" + ref
	if !useTags {
		remote, err := s.Git.GetRemote()
		if err != nil {
			return nil, err
		}
		if remote == nil {
			return nil, errors.New("returned git remote was empty")
		}
		fullRef = *remote + "/" + fullRef
	}
	return s.Git.GetLastCommitOnRef(fullRef)
}

func (s SemverHandler) refsToOrderedScopedVersions(refs []string) map[string][]Version {
	allVers := map[string][]Version{}
	for _, ref := range refs {
		s.Logger.Debugf("attempting to parse found version %s", ref)
		strVer := ref
		scope := ""
		if len(strings.Split(ref, "/")) > 1 {
			split := strings.Split(ref, "/")
			strVer = split[len(split)-1]
			scope = strings.Join(split[:len(split)-1], "/")
			if scope != "" {
				s.Logger.Debugf("detected version %s for scope %s", ref, scope)
			}
		}

		sver, err := semver.ParseTolerant(strVer)
		if err != nil {
			s.Logger.Infof("could not parse %s to semantic version", sver)
			s.Logger.Info(err)
			continue
		}

		s.Logger.Infof("version %s added to comparisons", ref)

		if _, ok := allVers[scope]; !ok {
			allVers[scope] = []Version{}
		}
		allVers[scope] = append(allVers[scope], Version{
			VersionString: strVer,
			Version:       sver,
		})
	}

	for _, vers := range allVers {
		sort.Slice(vers, func(i, j int) bool {
			return vers[i].Version.GT(vers[j].Version)
		})
	}

	return allVers
}

func (s SemverHandler) parseConventionalCommit(commitMesage string) (
	conventionalcommits.Message,
	*conventionalcommits.ConventionalCommit,
	error) {

	machine := parser.NewMachine(conventionalcommits.WithTypes(conventionalcommits.TypesConventional), conventionalcommits.WithBestEffort())
	msg, err := machine.Parse([]byte(commitMesage))
	if err != nil {
		return nil, nil, err
	}

	if !msg.Ok() {
		return nil, nil, errors.New("commit did not match conventional commit specification")
	}

	if cc, ok := msg.(*conventionalcommits.ConventionalCommit); ok {
		return msg, cc, nil
	}

	return msg, nil, errors.New("failed to cast conventional commit message to conventional commit type")
}

func (s SemverHandler) determineIncrementFromCommits(commits []string) map[string]int {
	scopedIncrements := map[string]int{}
	for _, commit := range commits {
		message, err := s.Git.GetCommitMessageBody(commit)
		if err != nil {
			s.Logger.Error(err)
			continue
		}
		if message == nil {
			s.Logger.Errorf("did not find a commit message body for %s", commit)
			continue
		}

		s.Logger.Debugf("try to determine increment from %s", commit)
		s.Logger.Debug(*message)

		msg, cc, err := s.parseConventionalCommit(*message)
		if err != nil {
			s.Logger.Info(err)
			continue
		}

		scope := ""
		if cc.Scope != nil {
			scope = *cc.Scope
		}

		if _, ok := scopedIncrements[scope]; !ok {
			scopedIncrements[scope] = 0
		}

		if msg.IsBreakingChange() {
			scopedIncrements[scope] = majorIncrement
			break
		}

		switch cc.Type {
		case "feat", "refactor":
			if scopedIncrements[scope] < minorIncrement {
				scopedIncrements[scope] = minorIncrement
			}
		case "fix", "chore", "perf", "docs", "style":
			if scopedIncrements[scope] < patchIncrement {
				scopedIncrements[scope] = patchIncrement
			}
		case "build", "ci", "test":
			if scopedIncrements[scope] < buildIncrement {
				scopedIncrements[scope] = buildIncrement
			}
		default:
			s.Logger.Infof("conventional commit type '%s' not implemented", cc.Type)
		}
	}

	return scopedIncrements
}

func (s SemverHandler) updateVersion(scope string, increment int, incomingVersion semver.Version, isPrerelease bool, prereleasePrefix, buildID *string) (*semver.Version, error) {
	switch increment {
	case majorIncrement:
		incomingVersion.Major++
		incomingVersion.Minor = 0
		incomingVersion.Patch = 0
		incomingVersion.Build = []string{}
	case minorIncrement:
		incomingVersion.Minor++
		incomingVersion.Patch = 0
		incomingVersion.Build = []string{}
	case patchIncrement:
		incomingVersion.Patch++
		incomingVersion.Build = []string{}
	case buildIncrement:
		if buildID != nil {
			incomingVersion.Build = append(incomingVersion.Build, *buildID)
		} else {
			buildNumber := 1
			if len(incomingVersion.Build) > 0 {
				buildID := strings.Split(incomingVersion.Build[0], "-")
				if len(buildID) >= 1 {
					pastBuildNumber, err := strconv.Atoi(buildID[1])
					if err != nil {
						s.Logger.Warn(err)
					} else {
						pastBuildNumber++
						buildNumber = pastBuildNumber
					}
				}
			}
			incomingVersion.Build = []string{fmt.Sprintf("build-%d", buildNumber)}

		}
	}

	if isPrerelease {
		prereleaseVer := uint64(1)

		if len(incomingVersion.Pre) > 0 {
			if incomingVersion.Pre[0].IsNum {
				prereleaseVer = incomingVersion.Pre[0].VersionNum
			} else {
				splitPrerelease := strings.Split(incomingVersion.Pre[0].VersionStr, "-")
				splitPrereleaseVer := splitPrerelease[len(splitPrerelease)-1]
				splitPrereleaseVerParsed, err := strconv.ParseUint(splitPrereleaseVer, 10, 64)
				if err != nil {
					s.Logger.Infof("could not parse number on existing prerelease version %s", splitPrereleaseVer)
					s.Logger.Info(err)
				} else {
					prereleaseVer = splitPrereleaseVerParsed
				}
			}
		}

		if prereleasePrefix != nil {
			incomingVersion.Pre = append(incomingVersion.Pre, semver.PRVersion{
				VersionStr: fmt.Sprintf("%s-%d", *prereleasePrefix, prereleaseVer),
				IsNum:      false,
			})
		} else {
			incomingVersion.Pre = append(incomingVersion.Pre, semver.PRVersion{
				VersionNum: prereleaseVer,
				IsNum:      true,
			})
		}
	}

	return &incomingVersion, nil
}

func (s SemverHandler) getBranchAndRemote(lookupBranch *string) (string, string, error) {
	branch := ""
	if lookupBranch != nil {
		branch = *lookupBranch
	} else {
		currentBranch, err := s.Git.GetCurrentBranch()
		if err != nil {
			s.Logger.Warn("failed to get the current git branch")
			return "", "", err
		}
		if currentBranch == nil {
			s.Logger.Warn("failed to get the current git branch")
			return "", "", errors.New("was not able to get the current branch on git")
		}

		branch = *currentBranch
	}

	remote, err := s.Git.GetRemote()
	if err != nil {
		return branch, "", err
	}
	if remote == nil {
		return branch, "", errors.New("returned git remote was empty")
	}

	return branch, *remote, nil
}

type GetReleasedVersionsOptions struct {
	SkipFetch bool
	UseTags   bool
}

func (s SemverHandler) GetReleasedVersions(opts GetReleasedVersionsOptions) (map[string][]Version, error) {
	if !opts.SkipFetch {
		if err := s.Git.Fetch(); err != nil {
			s.Logger.Error("failed to run git fetch against target repository")
			return nil, err
		}
	}

	refType := "heads"
	if opts.UseTags {
		refType = "tags"
	}

	refs, err := s.Git.ListRemoteRefs(refType)
	if err != nil {
		return nil, err
	}

	var releasedRefs []string
	for _, ref := range refs {
		prefixRegex := regexp.MustCompile(fmt.Sprintf("^%s/", s.getBranchPrefix()))
		if prefixRegex.MatchString(ref) {
			refSplit := prefixRegex.Split(ref, 2)
			s.Logger.Debugf("ref %s matched filter and trim, capturing %s", ref, refSplit[1])
			releasedRefs = append(releasedRefs, refSplit[1])
		}
	}

	scopedRefs := s.refsToOrderedScopedVersions(releasedRefs)

	return scopedRefs, nil
}

type VersionOptions struct {
	UseTags          bool
	Branch           *string
	BuildID          *string
	IsPrerelease     bool
	PrereleasePrefix *string
	ReleasePrefix    *string
	VersionPrefix    *string
}

func (s SemverHandler) GetInitialVersions(opts VersionOptions) (map[string]Version, error) {
	refType := "heads"
	if opts.UseTags {
		refType = "tags"
	}

	branch, remote, err := s.getBranchAndRemote(opts.Branch)
	if err != nil {
		return nil, err
	}

	latestCommit, err := s.Git.GetLastCommitOnRef(fmt.Sprintf("%s heads/%s", remote, branch))
	if err != nil {
		s.Logger.Error("failed to get latest commit for branch %s on %s", branch, remote)
		return nil, err
	}
	if latestCommit == nil {
		return nil, fmt.Errorf("latest commit for branch %s on %s returned blank", branch, remote)
	}

	commits, err := s.Git.ListCommits(*latestCommit)
	if err != nil {
		s.Logger.Error("failed to get list commits for branch %s on %s", branch, remote)
		return nil, err
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("no commits were found on branch %s on %s", branch, remote)
	}

	result := map[string]Version{}
	for scope, increment := range s.determineIncrementFromCommits(commits) {
		incomingVersion, err := s.updateVersion(scope, increment, semver.MustParse("0.0.0"), opts.IsPrerelease, opts.PrereleasePrefix, opts.BuildID)
		if err != nil {
			s.Logger.Errorf("failed to update the version for scope %s", scope)
			s.Logger.Error(err)
			continue
		}
		if incomingVersion == nil {
			s.Logger.Errorf("failed to get updated the version for scope %s", scope)
			continue
		}

		sb := strings.Builder{}
		if opts.ReleasePrefix != nil {
			sb.WriteString(fmt.Sprintf("%s/", *opts.ReleasePrefix))
		}
		sb.WriteString(scope)
		if opts.VersionPrefix != nil {
			sb.WriteString(*opts.VersionPrefix)
		}

		releaseRef := sb.String()

		result[scope] = Version{
			VersionString: releaseRef + incomingVersion.String(),
			Version:       *incomingVersion,
			ReleaseCommit: *latestCommit,
			ReleaseString: fmt.Sprintf("refs/%s/%s", refType, releaseRef),
		}
	}

	return result, nil
}

func (s SemverHandler) EvaluateScopedVersions(scopedReleases map[string][]Version, opts VersionOptions) (map[string]Version, error) {
	result := map[string]Version{}
	for scope, refs := range scopedReleases {
		prefix := ""
		if scope != "" {
			prefix = scope + "/"
		}

		refType := "heads"
		if opts.UseTags {
			refType = "tags"
		}

		s.Logger.Debugf("determining versions for scope %s at path %s", scope, prefix)

		lastReleasedCommit, err := s.getLastCommitOnRef(prefix+refs[0].VersionString, opts.UseTags)

		s.Logger.Debugf("last commit determined for scope %s was %s", scope, lastReleasedCommit)

		if err != nil || lastReleasedCommit == nil {
			s.Logger.Errorf("failed to get latest commit for scope %s", scope)
			continue
		}

		trimmedLastReleasedCommit := strings.TrimSpace(*lastReleasedCommit)

		branch, remote, err := s.getBranchAndRemote(opts.Branch)
		if err != nil {
			return nil, err
		}

		commits, err := s.Git.ListCommits(trimmedLastReleasedCommit + ".." + remote + "/" + branch)
		if err != nil {
			s.Logger.Errorf("failed to list commits after %s for scope %s", trimmedLastReleasedCommit, commits)
			continue
		}

		increments := s.determineIncrementFromCommits(commits)
		if _, ok := increments[scope]; !ok {
			return nil, fmt.Errorf("failed to find an increment for the given scope %s", scope)
		}

		incomingVersion, err := s.updateVersion(scope, increments[scope], refs[0].Version, opts.IsPrerelease, opts.PrereleasePrefix, opts.BuildID)
		if err != nil {
			s.Logger.Errorf("failed to update the version for scope %s", scope)
			s.Logger.Error(err)
			continue
		}
		if incomingVersion == nil {
			s.Logger.Errorf("failed to get updated the version for scope %s", scope)
			continue
		}

		sb := strings.Builder{}
		if opts.ReleasePrefix != nil {
			sb.WriteString(fmt.Sprintf("%s/", *opts.ReleasePrefix))
		}
		sb.WriteString(scope)
		if opts.VersionPrefix != nil {
			sb.WriteString(*opts.VersionPrefix)
		}

		releaseRef := sb.String()

		mostRecentCommit := trimmedLastReleasedCommit
		if len(commits) > 0 {
			mostRecentCommit = commits[0]
		}

		result[scope] = Version{
			VersionString: releaseRef + incomingVersion.String(),
			Version:       *incomingVersion,
			ReleaseCommit: mostRecentCommit,
			ReleaseString: fmt.Sprintf("refs/%s/%s", refType, releaseRef),
		}
	}

	return result, nil
}
