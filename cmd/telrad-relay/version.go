package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type releaseVersion struct {
	core       [3]int
	prerelease []string
}

func compareReleaseVersions(current, available string) (int, error) {
	currentVersion, err := parseReleaseVersion(current)
	if err != nil {
		return 0, fmt.Errorf("running relay version %q is not release SemVer: %w", current, err)
	}
	availableVersion, err := parseReleaseVersion(available)
	if err != nil {
		return 0, fmt.Errorf("manifest version %q is not release SemVer: %w", available, err)
	}
	return currentVersion.compare(availableVersion), nil
}

func parseReleaseVersion(value string) (releaseVersion, error) {
	var parsed releaseVersion
	if value == "" || strings.Contains(value, "+") {
		return parsed, errors.New("must not be empty or contain build metadata")
	}
	parts := strings.SplitN(value, "-", 2)
	core := strings.Split(parts[0], ".")
	if len(core) != 3 {
		return parsed, errors.New("must contain major, minor, and patch numbers")
	}
	for index, identifier := range core {
		if !validNumericIdentifier(identifier) {
			return parsed, errors.New("core identifiers must be canonical non-negative integers")
		}
		number, err := strconv.Atoi(identifier)
		if err != nil {
			return parsed, errors.New("version number is too large")
		}
		parsed.core[index] = number
	}
	if len(parts) == 1 {
		return parsed, nil
	}
	parsed.prerelease = strings.Split(parts[1], ".")
	for _, identifier := range parsed.prerelease {
		if identifier == "" {
			return releaseVersion{}, errors.New("prerelease identifiers must not be empty")
		}
		for _, character := range identifier {
			if !(character >= '0' && character <= '9') && !(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') && character != '-' {
				return releaseVersion{}, errors.New("prerelease identifiers contain an invalid character")
			}
		}
		if numericIdentifier(identifier) && !validNumericIdentifier(identifier) {
			return releaseVersion{}, errors.New("numeric prerelease identifiers must not contain leading zeroes")
		}
		if numericIdentifier(identifier) {
			if _, err := strconv.Atoi(identifier); err != nil {
				return releaseVersion{}, errors.New("numeric prerelease identifier is too large")
			}
		}
	}
	return parsed, nil
}

func validNumericIdentifier(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	return numericIdentifier(value)
}

func numericIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func (version releaseVersion) compare(other releaseVersion) int {
	for index := range version.core {
		if version.core[index] < other.core[index] {
			return -1
		}
		if version.core[index] > other.core[index] {
			return 1
		}
	}
	if len(version.prerelease) == 0 && len(other.prerelease) == 0 {
		return 0
	}
	if len(version.prerelease) == 0 {
		return 1
	}
	if len(other.prerelease) == 0 {
		return -1
	}
	for index := 0; index < len(version.prerelease) && index < len(other.prerelease); index++ {
		left, right := version.prerelease[index], other.prerelease[index]
		if left == right {
			continue
		}
		leftNumeric, rightNumeric := numericIdentifier(left), numericIdentifier(right)
		if leftNumeric && rightNumeric {
			leftNumber, _ := strconv.Atoi(left)
			rightNumber, _ := strconv.Atoi(right)
			if leftNumber < rightNumber {
				return -1
			}
			return 1
		}
		if leftNumeric {
			return -1
		}
		if rightNumeric {
			return 1
		}
		if left < right {
			return -1
		}
		return 1
	}
	if len(version.prerelease) < len(other.prerelease) {
		return -1
	}
	if len(version.prerelease) > len(other.prerelease) {
		return 1
	}
	return 0
}
