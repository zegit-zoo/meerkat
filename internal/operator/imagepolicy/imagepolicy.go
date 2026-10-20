// Package imagepolicy decides which container image a MeerkatMob's
// instances may run (meerkat-mob#10). spec.image is a privilege
// boundary: whoever can create a MeerkatMob would otherwise choose the
// image its pods run, with the S3 and OIDC configuration the operator
// gives them. The operator's own flags name the default image and the
// repositories an override may come from; the webhook refuses anything
// else at admission and the reconciler refuses it again before it
// renders a Deployment.
package imagepolicy

import (
	"fmt"
	"strings"
)

// DefaultRepository is the meerkat server image every release
// publishes.
const DefaultRepository = "ghcr.io/zegit-zoo/meerkat"

// Policy is the default image and the allow-list for overrides.
type Policy struct {
	// Default is the image an empty spec.image runs. It is the
	// operator's own choice, so it is not checked against Allowed.
	Default string
	// Allowed lists the repositories an override may name. An entry
	// matches a repository exactly ("ghcr.io/zegit-zoo/meerkat"), or,
	// when it ends in "/", every repository below that path
	// ("registry.example.net/mirror/"). Empty refuses every override.
	Allowed []string
}

// New builds a policy from the operator's flags. Each allowed entry
// must name its registry host, so an unqualified "meerkat" (which a
// container runtime reads as docker.io/library/meerkat) cannot be
// listed by mistake.
func New(defaultImage string, allowed []string) (*Policy, error) {
	if defaultImage == "" {
		return nil, fmt.Errorf("the default server image is empty")
	}
	if _, err := repository(defaultImage); err != nil {
		return nil, fmt.Errorf("default server image: %w", err)
	}
	p := &Policy{Default: defaultImage}
	for _, a := range allowed {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		t := strings.TrimSuffix(a, "/")
		if strings.ContainsAny(a, "@ \t") || strings.LastIndex(t, ":") > strings.LastIndex(t, "/") {
			return nil, fmt.Errorf("allowed image repository %q must be a repository, without a tag or digest", a)
		}
		if !qualified(a) {
			return nil, fmt.Errorf("allowed image repository %q must start with a registry host (for example ghcr.io/…)", a)
		}
		p.Allowed = append(p.Allowed, a)
	}
	return p, nil
}

// Resolve returns the image a MeerkatMob with spec.image runs, or an
// error naming why the override is refused.
func (p *Policy) Resolve(image string) (string, error) {
	if image == "" {
		return p.Default, nil
	}
	if image == p.Default {
		return image, nil
	}
	repo, err := repository(image)
	if err != nil {
		return "", fmt.Errorf("spec.image: %w", err)
	}
	for _, a := range p.Allowed {
		if repo == a || (strings.HasSuffix(a, "/") && strings.HasPrefix(repo, a)) {
			return image, nil
		}
	}
	if len(p.Allowed) == 0 {
		return "", fmt.Errorf("spec.image %q is not allowed: this operator allows no image overrides (--allowed-images is empty); leave spec.image empty to run %s", image, p.Default)
	}
	return "", fmt.Errorf("spec.image %q is not allowed: its repository %q is not one this operator allows (%s); leave spec.image empty to run %s", image, repo, strings.Join(p.Allowed, ", "), p.Default)
}

// repository is image without its tag and digest. It refuses what a
// container runtime would read differently from a plain comparison:
// whitespace, a scheme, an empty path segment and a dot segment.
func repository(image string) (string, error) {
	if image == "" || strings.ContainsAny(image, " \t\r\n") || strings.Contains(image, "://") {
		return "", fmt.Errorf("%q is not an image reference", image)
	}
	repo := image
	if i := strings.Index(repo, "@"); i >= 0 {
		repo = repo[:i]
	}
	// A tag follows the last colon after the last slash; a colon
	// before it is a registry port.
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	for _, seg := range strings.Split(repo, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%q is not an image reference", image)
		}
	}
	return repo, nil
}

// qualified reports whether ref's first path segment is a registry host:
// it holds a dot or a port, or is localhost.
func qualified(ref string) bool {
	host, _, ok := strings.Cut(ref, "/")
	if !ok {
		return false
	}
	return strings.ContainsAny(host, ".:") || host == "localhost"
}
