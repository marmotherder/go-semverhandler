package semverhandler

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver"
	"github.com/leodido/go-conventionalcommits"
	"github.com/leodido/go-conventionalcommits/parser"
	"github.com/marmotherder/go-gitcliwrapper"
)

type Version struct {
	VersionString string
	Version       semver.Version
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
	VersionPrefix *string
}

func (s SemverHandler) getBranchPrefix() string {
	if s.ReleasePrefix != nil {
		return *s.ReleasePrefix + "/"
	}
	return ""
}

func (s SemverHandler) getOldest(scopedRefs map[string][]Version, branch, remote, latestCommit string, useTags bool) (oldestDT *time.Time, oldestVersion *string, oldestScope *string, err error) {
	if len(scopedRefs) <= 0 {
		var commits []string
		commits, err = s.Git.ListCommits(latestCommit)
		if err != nil {
			s.Logger.Errorf("failed to get list commits for branch %s on %s", branch, remote)
			return
		}
		if len(commits) == 0 {
			err = fmt.Errorf("no commits were found on branch %s on %s", branch, remote)
			return
		}

		oldestCommit := commits[len(commits)-1]
		oldestDT, err = s.Git.GetReferenceDateTime(commits[len(commits)-1])
		oldestVersion = &oldestCommit

		return
	}

	for scope, versions := range scopedRefs {
		if len(versions) == 0 {
			continue
		}
		version := versions[0]
		sb := strings.Builder{}
		sb.WriteString(s.getBranchPrefix())
		if scope != "" {
			sb.WriteString(scope)
			sb.WriteString("/")
		}
		if s.VersionPrefix != nil {
			sb.WriteString(*s.VersionPrefix)
		}

		sb.WriteString(version.VersionString)
		lookupVersion := sb.String()

		if !useTags {
			lookupVersion = remote + "/" + lookupVersion
		}

		var lastCommit *string
		lastCommit, err = s.Git.GetLastCommitOnRef(lookupVersion)
		if err != nil {
			s.Logger.Warnf("was unable to resolve the latest commit for %s", lookupVersion)
		}
		if lastCommit == nil {
			err = fmt.Errorf("was unable to find a latest commit for %s", lookupVersion)
			return
		}

		var dt *time.Time
		dt, err = s.Git.GetReferenceDateTime(lookupVersion)
		if err != nil {
			s.Logger.Warnf("was unable to resolve the last change on %s to a date time", lookupVersion)
			return
		}
		if dt == nil {
			err = fmt.Errorf("was unable to load the date time for the reference %s", lookupVersion)
			return
		}

		if oldestDT == nil || dt.Before(*oldestDT) {
			oldestDT = dt
			oldestVersion = &lookupVersion
			currentScope := scope
			oldestScope = &currentScope
		}
	}

	return
}

type ScopeData struct {
	Increment int
	Commits   []string
}

func (s SemverHandler) determineIncrementFromCommits(commits []string) map[string]ScopeData {
	results := map[string]ScopeData{}
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

		if _, ok := results[scope]; !ok {
			results[scope] = ScopeData{
				Increment: 0,
				Commits:   []string{},
			}
		}

		result := results[scope]

		result.Commits = append(result.Commits, commit)

		if msg.IsBreakingChange() {
			result.Increment = majorIncrement
		}

		if result.Increment < majorIncrement {
			switch cc.Type {
			case "feat", "refactor":
				if result.Increment < minorIncrement {
					result.Increment = minorIncrement
				}
			case "fix", "chore", "perf", "docs", "style":
				if result.Increment < patchIncrement {
					result.Increment = patchIncrement
				}
			case "build", "ci", "test":
				if result.Increment < buildIncrement {
					result.Increment = buildIncrement
				}
			default:
				s.Logger.Infof("conventional commit type '%s' not implemented", cc.Type)
			}
		}

		results[scope] = result
	}

	return results
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

		if s.VersionPrefix != nil {
			strVer = strings.Replace(strVer, *s.VersionPrefix, "", 1)
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

func (s SemverHandler) UpdateVersion(scope string, increment int, incomingVersion semver.Version, isPrerelease bool, prereleasePrefix, buildID *string) (*semver.Version, error) {
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

type GetExistingScopedReleases struct {
	SkipFetch bool
	UseTags   bool
}

func (s SemverHandler) GetExistingScopedReleases(opts GetExistingScopedReleases) (map[string][]Version, error) {
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
		prefixRegex := regexp.MustCompile(fmt.Sprintf("^%s", s.getBranchPrefix()))
		if prefixRegex.MatchString(ref) {
			refSplit := prefixRegex.Split(ref, 2)
			s.Logger.Debugf("ref %s matched filter and trim, capturing %s", ref, refSplit[1])
			releasedRefs = append(releasedRefs, refSplit[1])
		}
	}

	return s.refsToOrderedScopedVersions(releasedRefs), nil
}

type LoadNewAndUpdatedReleasesOptions struct {
	Branch  *string
	UseTags bool
}

type ScopeVersion struct {
	ScopeData
	Version *Version
}

func (s SemverHandler) LoadNewAndUpdatedReleases(scopedReleases map[string][]Version, opts LoadNewAndUpdatedReleasesOptions) (map[string]ScopeVersion, error) {
	branch, remote, err := s.getBranchAndRemote(opts.Branch)
	if err != nil {
		return nil, err
	}

	latestCommit, err := s.Git.GetLastCommitOnRef(fmt.Sprintf("%s/%s", remote, branch))
	if err != nil {
		s.Logger.Errorf("failed to get latest commit for branch %s on %s", branch, remote)
		return nil, err
	}
	if latestCommit == nil {
		return nil, fmt.Errorf("latest commit for branch %s on %s returned blank", branch, remote)
	}

	oldestDT, oldestVersion, oldestScope, err := s.getOldest(scopedReleases, branch, remote, *latestCommit, opts.UseTags)
	if err != nil {
		s.Logger.Errorf("failed to get the oldest commits against the %s on %s", branch, remote)
		return nil, err
	}
	if oldestDT == nil || oldestVersion == nil {
		return nil, fmt.Errorf("did not find the oldest commit aganst %s on %s", branch, remote)
	}

	oldestCommit, err := s.Git.GetLastCommitOnRef(*oldestVersion)
	if err != nil {
		s.Logger.Warnf("was unable to get the latest commit against %s", oldestVersion)
		return nil, err
	}
	if oldestCommit == nil {
		return nil, fmt.Errorf("latest commit against %s came back blank", *oldestVersion)
	}

	commits, err := s.Git.ListCommits(*oldestCommit + ".." + *latestCommit)
	if err != nil {
		s.Logger.Warn("was unable to resolve a list of commits")
		return nil, err
	}

	scopeData := s.determineIncrementFromCommits(commits)

	results := map[string]ScopeVersion{}
	for scope, data := range scopeData {
		if oldestScope != nil && scope == *oldestScope {
			version := scopedReleases[scope][0]
			scopeVersion := ScopeVersion{
				Version: &version,
			}
			scopeVersion.Increment = data.Increment
			scopeVersion.Commits = data.Commits
			results[scope] = scopeVersion
		}
		if _, ok := scopedReleases[scope]; !ok {
			scopeVersion := ScopeVersion{}
			scopeVersion.Increment = data.Increment
			scopeVersion.Commits = data.Commits
			results[scope] = scopeVersion
		}
	}

	for scope, versions := range scopedReleases {
		if _, ok := results[scope]; ok {
			continue
		}
		if len(versions) <= 0 {
			continue
		}

		version := versions[0]
		sb := strings.Builder{}
		if !opts.UseTags {
			sb.WriteString(remote)
			sb.WriteString("/")
		}
		sb.WriteString(s.getBranchPrefix())
		if scope != "" {
			sb.WriteString(scope)
			sb.WriteString("/")
		}
		if s.VersionPrefix != nil {
			sb.WriteString(*s.VersionPrefix)
		}

		sb.WriteString(version.VersionString)
		lookupVersion := sb.String()

		releaseCommit, err := s.Git.GetLastCommitOnRef(lookupVersion)
		if err != nil {
			s.Logger.Warnf("failed to get latest commit for reference %s on %s", lookupVersion, remote)
			s.Logger.Error(err)
			continue
		}
		if releaseCommit == nil {
			s.Logger.Warnf("no commits were found for reference %s on %s", lookupVersion, remote)
			continue
		}

		if *releaseCommit == *latestCommit {
			continue
		}

		releaseScopeData := s.determineIncrementFromCommits(commits)
		if _, ok := releaseScopeData[scope]; !ok {
			continue
		}

		scopedVersion := ScopeVersion{
			Version: &version,
		}
		scopedVersion.Increment = releaseScopeData[scope].Increment
		scopedVersion.Commits = releaseScopeData[scope].Commits
		results[scope] = scopedVersion
	}

	return results, nil
}

type CreateUpdateReleasesOptions struct {
	UseTags          bool
	IsPrerelease     bool
	PrereleasePrefix *string
	BuildID          *string
}

func (s SemverHandler) CreateUpdateReleases(scopedReleasesData map[string]ScopeVersion, opts CreateUpdateReleasesOptions) error {
	for scope, data := range scopedReleasesData {
		version := semver.MustParse("0.0.0")

		sb := strings.Builder{}
		sb.WriteString("refs/")
		if opts.UseTags {
			sb.WriteString("tags/")
		} else {
			sb.WriteString("heads/")
		}
		sb.WriteString(s.getBranchPrefix())
		if scope != "" {
			sb.WriteString(scope)
			sb.WriteString("/")
		}
		if data.Version != nil {
			version = data.Version.Version
		}
		if s.VersionPrefix != nil {
			sb.WriteString(*s.VersionPrefix)
		}

		incomingVersion, err := s.UpdateVersion(scope, data.Increment, version, opts.IsPrerelease, opts.PrereleasePrefix, opts.BuildID)
		if err != nil {
			s.Logger.Errorf("failed to get a new or updated version for scope %s", scope)
			return err
		}
		if incomingVersion == nil {
			return fmt.Errorf("attempt to generate a new or updaated version for %s came back empty", scope)
		}

		releasePrefix := sb.String()
		fullVersion := incomingVersion.String()
		releaseVersions := []string{fullVersion, fullVersion[0:3] + fullVersion[5:], fullVersion[0:1] + fullVersion[5:]}

		for _, releaseVersion := range releaseVersions {
			if err := s.Git.ForcePushSourceToTargetRef(data.Commits[0], releasePrefix+releaseVersion); err != nil {
				return err
			}
		}
	}

	return nil
}
