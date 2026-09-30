package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Cross-repository context bounds.
const (
	MaxContextRepos    = 20
	MaxContextClusters = 50
	MaxContextPaths    = 32
)

var (
	contextRepoName    = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+){0,3}$`)
	contextClusterName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Context declares other repositories the reviewer may read while reviewing a
// change of this one: the contracts, types, SDKs and clients it depends on or
// that depend on it. It is opt-in and release-ordered like Fuzz: a policy
// containing it makes an older binary exit 3, so Default never sets it.
//
// Repos lists repositories directly. Clusters names the repository clusters
// this repository belongs to: each is a named group of related repositories,
// defined in ClusterDefinitions or in the operator's --clusters file, whose
// members all become context (the repository itself excepted).
type Context struct {
	Repos              []ContextRepo      `json:"repos,omitempty"`
	Clusters           []string           `json:"clusters,omitempty"`
	ClusterDefinitions map[string]Cluster `json:"cluster_definitions,omitempty"`
}

// Cluster is a named group of related repositories, for example a service,
// its SDK and the shared types they exchange.
type Cluster struct {
	Description string        `json:"description,omitempty"`
	Repos       []ContextRepo `json:"repos"`
}

// ContextRepo identifies one context repository. In JSON it is either its
// name ("company/shared-types") or an object.
type ContextRepo struct {
	// Name is the repository as the forge names it (owner/repo). It also
	// locates the local checkout: --context-dir DIR/<name> or DIR/<repo>.
	Name string `json:"name"`
	// Ref is the revision to read: a branch, tag or commit ("HEAD" by default).
	Ref string `json:"ref,omitempty"`
	// URL is fetched, shallow, only with --fetch-context and only when no
	// local checkout was found. https and ssh only; no credentials in it.
	URL string `json:"url,omitempty"`
	// Paths restricts the files read to these globs, e.g. "api/**".
	Paths []string `json:"paths,omitempty"`
	// Role tells the reviewer what the repository is, e.g. "payment SDK".
	Role string `json:"role,omitempty"`
}

// UnmarshalJSON accepts a name or a strict object.
func (r *ContextRepo) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) > 0 && data[0] == '"' {
		var name string
		if err := json.Unmarshal(data, &name); err != nil {
			return err
		}
		*r = ContextRepo{Name: name}
		return nil
	}
	type plain ContextRepo
	var p plain
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return fmt.Errorf("context repository: %w", err)
	}
	*r = ContextRepo(p)
	return nil
}

func (r ContextRepo) validate() error {
	switch {
	case !contextRepoName.MatchString(r.Name) || len(r.Name) > 200 || strings.Contains(r.Name, ".."):
		return fmt.Errorf("context repository name %q must look like owner/repo", r.Name)
	case r.Ref != "" && (len(r.Ref) > 200 || strings.HasPrefix(r.Ref, "-") || strings.ContainsAny(r.Ref, " \t\r\n\x00~^:?*[\\") || strings.Contains(r.Ref, "..")):
		return fmt.Errorf("context repository %s: invalid ref %q", r.Name, r.Ref)
	case len(r.Role) > 200 || strings.ContainsAny(r.Role, "\r\n\x00"):
		return fmt.Errorf("context repository %s: role must be one line of at most 200 bytes", r.Name)
	case len(r.Paths) > MaxContextPaths:
		return fmt.Errorf("context repository %s: at most %d paths", r.Name, MaxContextPaths)
	}
	if r.URL != "" {
		if err := validContextURL(r.URL); err != nil {
			return fmt.Errorf("context repository %s: %w", r.Name, err)
		}
	}
	for _, p := range r.Paths {
		if p == "" || strings.Contains(p, "\\") || strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			return fmt.Errorf("context repository %s: invalid path glob %q", r.Name, p)
		}
		if _, err := path.Match(strings.ReplaceAll(p, "**", "*"), ""); err != nil {
			return fmt.Errorf("context repository %s: invalid path glob %q", r.Name, p)
		}
	}
	return nil
}

// validContextURL accepts https:// and ssh URLs (ssh:// or git@host:path)
// without embedded credentials, options or local paths.
func validContextURL(u string) error {
	if len(u) > 500 || strings.ContainsAny(u, " \t\r\n\x00") || strings.HasPrefix(u, "-") {
		return errors.New("invalid url")
	}
	switch {
	case strings.HasPrefix(u, "https://"):
		host, _, _ := strings.Cut(strings.TrimPrefix(u, "https://"), "/")
		if host == "" || strings.Contains(host, "@") {
			return errors.New("an https url must name a host and carry no credentials")
		}
	case strings.HasPrefix(u, "ssh://"):
		if strings.TrimPrefix(u, "ssh://") == "" {
			return errors.New("invalid ssh url")
		}
	case regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._/-]+$`).MatchString(u):
	default:
		return errors.New("url must be https:// or ssh")
	}
	return nil
}

func (c *Context) validate() error {
	if c == nil {
		return nil
	}
	if len(c.Repos) > MaxContextRepos || len(c.Clusters) > MaxContextClusters || len(c.ClusterDefinitions) > MaxContextClusters {
		return fmt.Errorf("context holds at most %d repositories and %d clusters", MaxContextRepos, MaxContextClusters)
	}
	for _, r := range c.Repos {
		if err := r.validate(); err != nil {
			return err
		}
	}
	for _, name := range c.Clusters {
		if !contextClusterName.MatchString(name) {
			return fmt.Errorf("invalid cluster name %q", name)
		}
	}
	return validateClusters(c.ClusterDefinitions)
}

func validateClusters(clusters map[string]Cluster) error {
	for name, cluster := range clusters {
		if !contextClusterName.MatchString(name) {
			return fmt.Errorf("invalid cluster name %q", name)
		}
		if len(cluster.Description) > 500 || len(cluster.Repos) == 0 || len(cluster.Repos) > MaxContextRepos {
			return fmt.Errorf("cluster %s needs 1..%d repositories and a description of at most 500 bytes", name, MaxContextRepos)
		}
		for _, r := range cluster.Repos {
			if err := r.validate(); err != nil {
				return fmt.Errorf("cluster %s: %w", name, err)
			}
		}
	}
	return nil
}

// ClusterFile is the operator's --clusters file: cluster definitions shared by
// every repository of an organization.
type ClusterFile struct {
	Clusters map[string]Cluster `json:"clusters"`
}

// DecodeClusters reads a --clusters file strictly.
func DecodeClusters(data []byte) (map[string]Cluster, error) {
	if len(data) > 1<<20 {
		return nil, errors.New("clusters file exceeds 1 MiB")
	}
	var f ClusterFile
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&f); err != nil {
		return nil, fmt.Errorf("clusters file: %w", err)
	}
	if len(f.Clusters) > MaxContextClusters {
		return nil, fmt.Errorf("clusters file defines more than %d clusters", MaxContextClusters)
	}
	return f.Clusters, validateClusters(f.Clusters)
}

// ResolvedRepo is one context repository with the clusters it came from.
type ResolvedRepo struct {
	ContextRepo
	Clusters []string
}

// ResolveContext lists the context repositories of a policy: its repos, then
// the members of its clusters, looked up in the policy's definitions and in
// shared (the --clusters file). A repository listed twice is merged: the first
// listing's settings win and the clusters accumulate. self, the reviewed
// repository's own name if known, is left out.
func (c *Context) ResolveContext(shared map[string]Cluster, self string) ([]ResolvedRepo, error) {
	if c == nil {
		return nil, nil
	}
	var out []ResolvedRepo
	index := map[string]int{}
	add := func(r ContextRepo, cluster string) {
		key := strings.ToLower(r.Name)
		if self != "" && strings.EqualFold(r.Name, self) {
			return
		}
		if i, ok := index[key]; ok {
			if cluster != "" && !contains(out[i].Clusters, cluster) {
				out[i].Clusters = append(out[i].Clusters, cluster)
			}
			return
		}
		index[key] = len(out)
		entry := ResolvedRepo{ContextRepo: r, Clusters: []string{}}
		if cluster != "" {
			entry.Clusters = append(entry.Clusters, cluster)
		}
		out = append(out, entry)
	}
	for _, r := range c.Repos {
		add(r, "")
	}
	for _, name := range c.Clusters {
		local, inPolicy := c.ClusterDefinitions[name]
		remote, inShared := shared[name]
		var cluster Cluster
		switch {
		case inPolicy && inShared:
			return nil, fmt.Errorf("cluster %s is defined both in the policy and in the clusters file", name)
		case inPolicy:
			cluster = local
		case inShared:
			cluster = remote
		default:
			known := make([]string, 0, len(shared))
			for k := range shared {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("cluster %s is not defined (policy cluster_definitions or --clusters file; known: %s)", name, strings.Join(known, ", "))
		}
		for _, r := range cluster.Repos {
			add(r, name)
		}
	}
	if len(out) > MaxContextRepos {
		return nil, fmt.Errorf("the context resolves to %d repositories, more than %d", len(out), MaxContextRepos)
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
