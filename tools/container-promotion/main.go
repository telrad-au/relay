package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const schemaVersion = 1

var (
	stableTagPattern  = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	revisionPattern   = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	imagePattern      = regexp.MustCompile(`^[a-z0-9.-]+(?::[0-9]+)?(?:/[a-z0-9._-]+)+$`)
	platformPattern   = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9]+(?:/[a-z0-9]+)?$`)
	requiredPlatforms = []string{
		"linux/amd64",
		"linux/arm64",
	}
)

type promotionMetadata struct {
	SchemaVersion      int      `json:"schemaVersion"`
	Prerelease         string   `json:"prerelease"`
	StableVersion      string   `json:"stableVersion"`
	Revision           string   `json:"revision"`
	Image              string   `json:"image"`
	Digest             string   `json:"digest"`
	Platforms          []string `json:"platforms"`
	WorkflowRunID      int64    `json:"workflowRunId"`
	WorkflowRunAttempt int      `json:"workflowRunAttempt"`
}

type prereleaseVersion struct {
	stable      string
	identifiers []string
}

type stringList []string

func (values *stringList) String() string {
	return strings.Join(*values, ",")
}

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		exitError("usage: container-promotion <validate|guard-stable|create|select> [options]")
	}

	var err error
	switch os.Args[1] {
	case "validate":
		err = runValidate(os.Args[2:], os.Stdout)
	case "guard-stable":
		err = runGuardStable(os.Args[2:], os.Stdout)
	case "create":
		err = runCreate(os.Args[2:], os.Stdout)
	case "select":
		err = runSelect(os.Args[2:], os.Stdout, os.Stderr)
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		exitError(err.Error())
	}
}

func runGuardStable(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("guard-stable", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	candidateTag := flags.String("candidate", "", "candidate stable release tag")
	if err := flags.Parse(args); err != nil {
		return err
	}
	candidate, err := parseStable(*candidateTag)
	if err != nil {
		return err
	}
	for _, existingTag := range flags.Args() {
		if existingTag == *candidateTag {
			continue
		}
		existing, err := parseStable(existingTag)
		if err != nil {
			return fmt.Errorf("existing %w", err)
		}
		if compareStable(candidate, existing) <= 0 {
			return fmt.Errorf("candidate stable tag %s must be newer than existing stable tag %s", *candidateTag, existingTag)
		}
	}
	_, err = fmt.Fprintln(output, *candidateTag)
	return err
}

func runValidate(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	prerelease := flags.String("prerelease", "", "prerelease tag")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("validate does not accept positional arguments")
	}
	version, err := parsePrerelease(*prerelease)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(output, version.stable)
	return err
}

func exitError(message string) {
	fmt.Fprintf(os.Stderr, "container-promotion: %s\n", message)
	os.Exit(1)
}

func runCreate(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("create", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	prerelease := flags.String("prerelease", "", "prerelease tag")
	revision := flags.String("revision", "", "source commit revision")
	image := flags.String("image", "", "container image name")
	digest := flags.String("digest", "", "container digest")
	workflowRunID := flags.Int64("workflow-run-id", 0, "GitHub Actions workflow run ID")
	workflowRunAttempt := flags.Int("workflow-run-attempt", 0, "GitHub Actions workflow run attempt")
	var platforms stringList
	flags.Var(&platforms, "platform", "container platform (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("create does not accept positional arguments")
	}

	version, err := parsePrerelease(*prerelease)
	if err != nil {
		return err
	}
	metadata := promotionMetadata{
		SchemaVersion:      schemaVersion,
		Prerelease:         *prerelease,
		StableVersion:      version.stable,
		Revision:           *revision,
		Image:              *image,
		Digest:             *digest,
		Platforms:          platforms,
		WorkflowRunID:      *workflowRunID,
		WorkflowRunAttempt: *workflowRunAttempt,
	}
	if err := validateMetadata(metadata, version.stable, *revision, *image); err != nil {
		return err
	}

	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(metadata)
}

func runSelect(args []string, output, diagnostics io.Writer) error {
	flags := flag.NewFlagSet("select", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	stable := flags.String("stable", "", "stable release tag")
	revision := flags.String("revision", "", "stable source commit revision")
	image := flags.String("image", "", "expected container image name")
	if err := flags.Parse(args); err != nil {
		return err
	}

	candidates, err := selectCandidates(*stable, *revision, *image, flags.Args(), diagnostics)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(candidates)
}

func selectCandidates(stable, revision, image string, paths []string, diagnostics io.Writer) ([]promotionMetadata, error) {
	if !stableTagPattern.MatchString(stable) {
		return nil, fmt.Errorf("stable tag must be SemVer without prerelease or build metadata")
	}
	if !revisionPattern.MatchString(revision) {
		return nil, fmt.Errorf("stable revision must be a lowercase Git commit SHA")
	}
	if !imagePattern.MatchString(image) {
		return nil, fmt.Errorf("image must be an untagged fully qualified image name")
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no prerelease contains container-promotion.json")
	}

	candidates := make([]promotionMetadata, 0, len(paths))
	seen := make(map[string]bool)
	for _, path := range paths {
		metadata, err := readMetadata(path)
		if err == nil {
			expectedTag := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			if metadata.Prerelease != expectedTag {
				err = fmt.Errorf("asset belongs to %s, not %s", expectedTag, metadata.Prerelease)
			}
		}
		if err == nil {
			err = validateMetadata(metadata, stable, revision, image)
		}
		if err == nil && seen[metadata.Prerelease] {
			err = fmt.Errorf("duplicate metadata for %s", metadata.Prerelease)
		}
		if err != nil {
			fmt.Fprintf(diagnostics, "Ignoring ineligible promotion metadata %s: %v\n", path, err)
			continue
		}
		seen[metadata.Prerelease] = true
		candidates = append(candidates, metadata)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no valid prerelease promotion metadata matches %s at %s", stable, revision)
	}

	sort.Slice(candidates, func(i, j int) bool {
		left, _ := parsePrerelease(candidates[i].Prerelease)
		right, _ := parsePrerelease(candidates[j].Prerelease)
		return comparePrerelease(left, right) > 0
	})
	return candidates, nil
}

func readMetadata(path string) (promotionMetadata, error) {
	file, err := os.Open(path)
	if err != nil {
		return promotionMetadata{}, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var metadata promotionMetadata
	if err := decoder.Decode(&metadata); err != nil {
		return promotionMetadata{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return promotionMetadata{}, fmt.Errorf("metadata contains more than one JSON value")
		}
		return promotionMetadata{}, err
	}
	return metadata, nil
}

func validateMetadata(metadata promotionMetadata, stable, revision, image string) error {
	if metadata.SchemaVersion != schemaVersion {
		return fmt.Errorf("schemaVersion must be %d", schemaVersion)
	}
	version, err := parsePrerelease(metadata.Prerelease)
	if err != nil {
		return err
	}
	if metadata.StableVersion != version.stable {
		return fmt.Errorf("stableVersion %q does not match prerelease base %q", metadata.StableVersion, version.stable)
	}
	if metadata.StableVersion != stable {
		return fmt.Errorf("stableVersion %q does not match requested release %q", metadata.StableVersion, stable)
	}
	if !revisionPattern.MatchString(metadata.Revision) {
		return fmt.Errorf("revision must be a lowercase Git commit SHA")
	}
	if metadata.Revision != revision {
		return fmt.Errorf("revision %q does not match stable source %q", metadata.Revision, revision)
	}
	if !imagePattern.MatchString(metadata.Image) {
		return fmt.Errorf("image must be an untagged fully qualified image name")
	}
	if metadata.Image != image {
		return fmt.Errorf("image %q does not match expected image %q", metadata.Image, image)
	}
	if !digestPattern.MatchString(metadata.Digest) {
		return fmt.Errorf("digest must be canonical lowercase sha256")
	}
	if err := validatePlatforms(metadata.Platforms); err != nil {
		return err
	}
	if metadata.WorkflowRunID <= 0 {
		return fmt.Errorf("workflowRunId must be positive")
	}
	if metadata.WorkflowRunAttempt <= 0 {
		return fmt.Errorf("workflowRunAttempt must be positive")
	}
	return nil
}

func validatePlatforms(platforms []string) error {
	seen := make(map[string]bool, len(platforms))
	for _, platform := range platforms {
		if !platformPattern.MatchString(platform) {
			return fmt.Errorf("invalid platform %q", platform)
		}
		if seen[platform] {
			return fmt.Errorf("duplicate platform %q", platform)
		}
		seen[platform] = true
	}
	for _, required := range requiredPlatforms {
		if !seen[required] {
			return fmt.Errorf("required platform %q is missing", required)
		}
	}
	return nil
}

func parsePrerelease(tag string) (prereleaseVersion, error) {
	parts := strings.SplitN(tag, "-", 2)
	if len(parts) != 2 || !stableTagPattern.MatchString(parts[0]) || parts[1] == "" {
		return prereleaseVersion{}, fmt.Errorf("prerelease tag %q is not valid SemVer", tag)
	}
	identifiers := strings.Split(parts[1], ".")
	for _, identifier := range identifiers {
		if identifier == "" {
			return prereleaseVersion{}, fmt.Errorf("prerelease tag %q contains an empty identifier", tag)
		}
		for _, character := range identifier {
			if !((character >= '0' && character <= '9') || (character >= 'A' && character <= 'Z') || (character >= 'a' && character <= 'z') || character == '-') {
				return prereleaseVersion{}, fmt.Errorf("prerelease tag %q contains an invalid identifier", tag)
			}
		}
		if isNumeric(identifier) && len(identifier) > 1 && identifier[0] == '0' {
			return prereleaseVersion{}, fmt.Errorf("prerelease tag %q contains a numeric identifier with a leading zero", tag)
		}
	}
	return prereleaseVersion{stable: parts[0], identifiers: identifiers}, nil
}

func parseStable(tag string) ([3]string, error) {
	matches := stableTagPattern.FindStringSubmatch(tag)
	if matches == nil {
		return [3]string{}, fmt.Errorf("stable tag %q must be SemVer without prerelease or build metadata", tag)
	}
	return [3]string{matches[1], matches[2], matches[3]}, nil
}

func compareStable(left, right [3]string) int {
	for index := range left {
		if len(left[index]) != len(right[index]) {
			return compareInts(len(left[index]), len(right[index]))
		}
		if comparison := strings.Compare(left[index], right[index]); comparison != 0 {
			return comparison
		}
	}
	return 0
}

func comparePrerelease(left, right prereleaseVersion) int {
	for index := 0; index < len(left.identifiers) && index < len(right.identifiers); index++ {
		leftIdentifier := left.identifiers[index]
		rightIdentifier := right.identifiers[index]
		leftNumeric := isNumeric(leftIdentifier)
		rightNumeric := isNumeric(rightIdentifier)
		switch {
		case leftNumeric && rightNumeric:
			if len(leftIdentifier) != len(rightIdentifier) {
				if len(leftIdentifier) < len(rightIdentifier) {
					return -1
				}
				return 1
			}
			if comparison := strings.Compare(leftIdentifier, rightIdentifier); comparison != 0 {
				return comparison
			}
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		default:
			if comparison := strings.Compare(leftIdentifier, rightIdentifier); comparison != 0 {
				return comparison
			}
		}
	}
	return compareInts(len(left.identifiers), len(right.identifiers))
}

func isNumeric(value string) bool {
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

func compareInts(left, right int) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}
