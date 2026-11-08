package gitdiff

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var (
	shaPattern       = regexp.MustCompile(`^(?i)[a-f0-9]{4,40}$`)
	forbiddenPattern = regexp.MustCompile(`[~^:?*\[\\\x00-\x20\x7F]`)
	specialArgs      = []string{"working", "staged", "."}
)

// IsSpecialArg reports whether s names the working tree, index, or both.
func IsSpecialArg(s string) bool {
	return slices.Contains(specialArgs, s)
}

// ValidateCommitish checks that a commit-ish looks like a hash, HEAD/@, a
// valid ref name, or a special argument, optionally followed by ^/~ suffixes.
func ValidateCommitish(commitish string) bool {
	trimmed := strings.TrimSpace(commitish)
	if trimmed == "" {
		return false
	}
	if IsSpecialArg(trimmed) {
		return true
	}
	base := stripRevisionSuffix(trimmed)
	if base == "" {
		return false
	}
	if shaPattern.MatchString(base) || base == "HEAD" || base == "@" {
		return true
	}
	return isValidBranchName(base)
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func stripRevisionSuffix(s string) string {
	end := len(s)
	for end > 0 {
		c := s[end-1]
		if c == '^' || c == '~' {
			end--
			continue
		}
		if !isDigit(c) {
			break
		}
		start := end - 1
		for start > 0 && isDigit(s[start-1]) {
			start--
		}
		if start == 0 || (s[start-1] != '^' && s[start-1] != '~') {
			break
		}
		end = start - 1
	}
	return s[:end]
}

func isValidBranchName(name string) bool {
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") ||
		strings.Contains(name, "//") || strings.HasPrefix(name, "/") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") ||
		forbiddenPattern.MatchString(name) {
		return false
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

// ValidateDiffArguments checks a target/base pair. An empty base means none.
func ValidateDiffArguments(target, base string, hasBase bool) error {
	if !ValidateCommitish(target) {
		return fmt.Errorf("Invalid target commit-ish format")
	}
	if hasBase && !ValidateCommitish(base) {
		return fmt.Errorf("Invalid base commit-ish format")
	}
	if base != "" && IsSpecialArg(base) && !(base == "staged" && target == "working") {
		return fmt.Errorf("Special arguments (working, staged, .) are only allowed as target, not base. Got base: %s", base)
	}
	if hasBase && target == base {
		return fmt.Errorf("Cannot compare %s with itself", target)
	}
	if target == "working" && base != "" && base != "staged" {
		return fmt.Errorf(`"working" shows unstaged changes and cannot be compared with another commit. Use "." instead to compare all uncommitted changes with a specific commit.`)
	}
	return nil
}

// ShortHash abbreviates a hash to 7 characters.
func ShortHash(hash string) string {
	if len(hash) > 7 {
		return hash[:7]
	}
	return hash
}
