package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	humane "github.com/sierrasoftworks/humane-errors-go"
)

// Source resolves the latest version identifier for an upstream artifact. Its
// return value is written verbatim into the bound Nomad job input variable, so
// each implementation is responsible for producing a value the job's HCL can
// consume directly (a release tag, a "tag@sha256:..." reference, and so on).
type Source interface {
	// Latest returns the current newest version string for the source.
	Latest(ctx context.Context) (string, error)
	// scheme reports the source's spec scheme, for telemetry attributes.
	scheme() string
}

// sourceFactory builds a Source from the locator and options of a spec. The
// locator is the portion of the spec after "<scheme>:" and before any "?",
// and opts holds the parsed query string.
type sourceFactory func(locator string, opts url.Values) (Source, humane.Error)

// sourceFactories maps a spec scheme to its factory. Only registered schemes
// are accepted, so a job cannot direct the service at an arbitrary protocol.
var sourceFactories = map[string]sourceFactory{}

func registerSource(scheme string, f sourceFactory) {
	sourceFactories[scheme] = f
}

// sourceHTTPClient is the shared client used by all sources. Individual
// requests still set context deadlines; the client timeout is a backstop.
var sourceHTTPClient = &http.Client{Timeout: 30 * time.Second}

// parseSource turns a spec of the form "<scheme>:<locator>[?<options>]" into a
// Source. The scheme selects the implementation and the remainder is
// interpreted by that implementation.
func parseSource(spec string) (Source, humane.Error) {
	scheme, rest, ok := strings.Cut(spec, ":")
	if !ok || scheme == "" {
		return nil, humane.New(fmt.Sprintf("update source spec %q is missing a scheme", spec),
			"Specify a source as <scheme>:<locator>, e.g. github:OWNER/REPO or docker:REGISTRY/IMAGE:TAG.")
	}

	locator := rest
	var opts url.Values
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		locator = rest[:q]
		parsed, err := url.ParseQuery(rest[q+1:])
		if err != nil {
			return nil, humane.Wrap(err, fmt.Sprintf("update source spec %q has invalid options", spec),
				"Options are a URL query string, e.g. github:OWNER/REPO?prefix=v2.")
		}
		opts = parsed
	}

	factory, ok := sourceFactories[scheme]
	if !ok {
		return nil, humane.New(fmt.Sprintf("unknown update source scheme %q in spec %q", scheme, spec),
			"Supported schemes are github and docker.")
	}
	return factory(locator, opts)
}

// envOr returns the value of the named environment variable, or fallback when
// it is unset or empty.
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
