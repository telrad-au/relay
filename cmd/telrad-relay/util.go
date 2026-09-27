package main

import (
	"mime"
	"net/url"
	"strings"
)

func parseURL(value string) (*url.URL, error) { return url.Parse(strings.TrimSpace(value)) }

func mediaTypeEquals(header, expected string) bool {
	mediaType, _, err := mime.ParseMediaType(header)
	return err == nil && strings.EqualFold(mediaType, expected)
}
