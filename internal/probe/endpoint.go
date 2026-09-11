package probe

import (
	"fmt"
	"net/url"
	"strings"
)

func parseEndpointURL(raw string) (url.URL, error) {
	if strings.Contains(raw, "#") {
		return url.URL{}, fmt.Errorf("fragments are not allowed")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return url.URL{}, fmt.Errorf("parse %q: %w", raw, err)
	}
	if err := validateEndpointURL(*parsed); err != nil {
		return url.URL{}, err
	}
	return *parsed, nil
}

func validateEndpointURL(endpoint url.URL) error {
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if endpoint.Host == "" {
		return fmt.Errorf("host is required")
	}
	if endpoint.User != nil {
		return fmt.Errorf("userinfo is not allowed")
	}
	if endpoint.Fragment != "" || endpoint.RawFragment != "" {
		return fmt.Errorf("fragments are not allowed")
	}
	return nil
}
