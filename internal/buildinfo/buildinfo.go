package buildinfo

import (
	"encoding/hex"
	"errors"
	"fmt"
	"unicode"
	"unicode/utf8"
)

var (
	releaseVersionHex   string
	developmentCommit   string
	developmentModified string
)

func Text() (string, error) {
	return format(releaseVersionHex, developmentCommit, developmentModified)
}

func format(release, commit, modified string) (string, error) {
	hasRelease := release != ""
	hasDevelopment := commit != "" || modified != ""
	if hasRelease == hasDevelopment {
		return "", errors.New("build identity is incomplete or mixed")
	}
	if hasRelease {
		version, err := decodeReleaseVersion(release)
		if err != nil {
			return "", err
		}
		return "transferlanes " + version + "\n", nil
	}
	if !validRevision(commit) || modified != "true" && modified != "false" {
		return "", errors.New("development build identity is invalid")
	}
	return fmt.Sprintf("transferlanes devel commit=%s modified=%s\n", commit, modified), nil
}

func decodeReleaseVersion(encoded string) (string, error) {
	if len(encoded)%2 != 0 || !lowercaseHex(encoded) {
		return "", errors.New("release version transport is invalid")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) == 0 || !utf8.Valid(decoded) {
		return "", errors.New("release version transport is invalid")
	}
	version := string(decoded)
	for _, value := range version {
		if unicode.Is(unicode.Cc, value) || unicode.Is(unicode.Zl, value) || unicode.Is(unicode.Zp, value) {
			return "", errors.New("release version is not stable single-line text")
		}
	}
	return version, nil
}

func validRevision(value string) bool {
	return (len(value) == 40 || len(value) == 64) && lowercaseHex(value)
}

func lowercaseHex(value string) bool {
	for index := range len(value) {
		character := value[index]
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}
