package builtin

import (
	"regexp"
	"slices"

	"github.com/dlclark/regexp2"
)

var LOOK_SYNTAX = []string{"(?=", "(?!", "(?<=", "(?<!"}

type regexSelector struct {
	goRegex   *regexp.Regexp
	lookRegex *regexp2.Regexp
}

func compileRegex(pattern string) (*regexSelector, error) {
	if slices.Contains(LOOK_SYNTAX, pattern) {
		regex, err := regexp2.Compile(pattern, regexp2.None)
		return &regexSelector{
			lookRegex: regex,
		}, err
	}
	regex, err := regexp.Compile(pattern)
	return &regexSelector{
		goRegex: regex,
	}, err
}

func (r *regexSelector) MatchString(s string) (bool, error) {
	if r.goRegex != nil {
		return r.goRegex.MatchString(s), nil
	}
	return r.lookRegex.MatchString(s)
}

func (r *regexSelector) FindStringSubmatch(s string) []string {
	if r.goRegex != nil {
		return r.goRegex.FindStringSubmatch(s)
	}
	// regexp2 does not have this ability, until we need to handle it with the matches ignore.
	return []string{}
}

func (r *regexSelector) FindStringMatch(s string) (string, int, error) {
	if r.goRegex != nil {
		matchString := r.goRegex.FindString(s)
		matchIndex := r.goRegex.FindStringIndex(s)
		if matchIndex == nil || len(matchIndex) < 1 {
			return "", 0, nil
		}
		return matchString, matchIndex[0], nil
	}
	match, err := r.lookRegex.FindStringMatch(s)
	if err != nil || match == nil {
		return "", 0, err
	}
	return match.String(), match.Index, err
}
