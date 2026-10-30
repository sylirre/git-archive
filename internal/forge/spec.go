package forge

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/sylirre/git-archive/internal/safe"
)

// Provider names.
const (
	GitHub = "github"
	GitLab = "gitlab"
	Gitea  = "gitea"
	Git    = "git" // any git server, single repositories only
)

// Providers lists the accepted --provider values.
var Providers = []string{GitHub, GitLab, Gitea, Git}

// Source types.
const (
	TypeOwner = "owner" // organisation, user or group: all its repositories
	TypeRepo  = "repo"  // a single repository
)

// defaultHosts are the public instances used when only a provider and a
// name are given ("--provider github termux").
var defaultHosts = map[string]string{GitHub: "github.com", GitLab: "gitlab.com", Gitea: "codeberg.org"}

// Spec is a parsed, validated source specification.
type Spec struct {
	Provider string
	Scheme   string // https, http (insecure), ssh, git (insecure) or file
	Host     string // lower-case host[:port]; "localhost" for file URLs
	Path     string // "termux", "termux/termux-packages", "group/sub/project"
	Type     string // TypeOwner, TypeRepo, or "" until resolved (GitLab)
	URL      string // URL to record; for Git sources the exact clone URL
	API      string // API base URL override
	Insecure bool   // plain-text transports allowed
	TokenEnv string
	// Notes are human-readable remarks produced while parsing, such as an
	// http:// URL having been upgraded to https://.
	Notes []string
}

// Name is the source identity used in the configuration: host/path.
func (s Spec) Name() string { return s.Host + "/" + s.Path }

// DetectProvider guesses the provider from a host name.
func DetectProvider(host string) string {
	h, _, _ := strings.Cut(host, ":")
	switch {
	case h == "github.com":
		return GitHub
	case h == "gitlab.com" || strings.HasPrefix(h, "gitlab."):
		return GitLab
	case h == "codeberg.org" || strings.HasPrefix(h, "gitea.") || strings.HasPrefix(h, "forgejo."):
		return Gitea
	}
	return ""
}

// ParseSpec interprets a source argument. provider may be empty (detect
// from the host). insecure permits http:// and git:// transports.
//
// Accepted forms:
//
//	https://github.com/termux               owner (organisation or user)
//	http://github.com/termux/termux-app     repository (upgraded to https)
//	github.com/termux                       scheme may be omitted
//	--provider github termux                provider default host
//	git@github.com:termux/termux-app.git    ssh, single repository
//	ssh://git@host:2222/path/repo.git       ssh, single repository
//	https://git.example.com/a/b.git         any git server, single repository
//	/srv/git/repo.git, file:///srv/...      local repository
func ParseSpec(arg, provider string, insecure bool) (Spec, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return Spec{}, fmt.Errorf("empty source")
	}
	for _, r := range arg {
		if r < ' ' || r == 0x7f || r == ' ' {
			return Spec{}, fmt.Errorf("source %q contains whitespace or control characters", safe.Term(arg))
		}
	}
	if provider != "" && !validProvider(provider) {
		return Spec{}, fmt.Errorf("unknown provider %q (known: %s)", provider, strings.Join(Providers, ", "))
	}
	s := Spec{Provider: provider, Insecure: insecure}
	var err error
	switch {
	case strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../"):
		err = s.parseLocal(arg)
	case strings.Contains(arg, "://"):
		err = s.parseURL(arg)
	case isSCP(arg):
		err = s.parseSCP(arg)
	case provider != "" && provider != Git && !looksLikeHost(arg):
		host := defaultHosts[provider]
		err = s.parseURL("https://" + host + "/" + arg)
	case looksLikeHost(arg):
		err = s.parseURL("https://" + arg)
	default:
		return Spec{}, fmt.Errorf("cannot tell where %q is hosted: give a URL (https://github.com/%s) or --provider", safe.Term(arg), safe.Term(arg))
	}
	if err != nil {
		return Spec{}, err
	}
	return s, s.finish()
}

func validProvider(p string) bool {
	for _, k := range Providers {
		if k == p {
			return true
		}
	}
	return false
}

func looksLikeHost(arg string) bool {
	first, _, _ := strings.Cut(arg, "/")
	return strings.Contains(first, ".") || strings.HasPrefix(first, "localhost")
}

// isSCP recognises scp-like ssh syntax: user@host:path. The user part is
// required so that "host:8080/path" is read as a host with a port.
func isSCP(arg string) bool {
	at := strings.IndexByte(arg, '@')
	colon := strings.IndexByte(arg, ':')
	slash := strings.IndexByte(arg, '/')
	return at > 0 && colon > at && (slash < 0 || colon < slash)
}

func (s *Spec) parseSCP(arg string) error {
	userHost, p, _ := strings.Cut(arg, ":")
	user, host, _ := strings.Cut(userHost, "@")
	if err := safe.Name(user); err != nil {
		return fmt.Errorf("ssh user: %w", err)
	}
	s.Scheme = "ssh"
	s.Host = strings.ToLower(host)
	s.Path = strings.Trim(p, "/")
	s.Type = TypeRepo
	s.URL = arg
	return nil
}

func (s *Spec) parseLocal(arg string) error {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return fmt.Errorf("local repository %s does not exist", abs)
	}
	return s.parseURL("file://" + filepath.ToSlash(abs))
}

func (s *Spec) parseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL %q: %v", safe.Term(raw), err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("URL %q must not contain a query or fragment", safe.Term(raw))
	}
	if u.User != nil {
		if _, hasPw := u.User.Password(); hasPw {
			return fmt.Errorf("URL %q contains a password; credentials must not be stored in the archive configuration, use a token environment variable instead", u.Redacted())
		}
	}
	s.Scheme = strings.ToLower(u.Scheme)
	s.Host = strings.ToLower(u.Host)
	p := strings.Trim(u.Path, "/")
	switch s.Scheme {
	case "https":
	case "http":
		known := s.Provider != "" && s.Provider != Git || DetectProvider(s.Host) != ""
		switch {
		case s.Insecure:
		case known:
			s.Scheme = "https"
			s.Notes = append(s.Notes, "using https:// instead of http://")
		default:
			return fmt.Errorf("refusing unencrypted http:// for %s; use https:// or --insecure", safe.Term(s.Host))
		}
	case "git":
		if !s.Insecure {
			return fmt.Errorf("git:// is unauthenticated and unencrypted; use https:// or --insecure")
		}
		s.Type = TypeRepo
	case "ssh":
		if u.User != nil {
			if err := safe.Name(u.User.Username()); err != nil {
				return fmt.Errorf("ssh user: %w", err)
			}
		}
		s.Type = TypeRepo
	case "file":
		s.Host = "localhost"
		s.Provider = Git
		s.Type = TypeRepo
		s.URL = "file://" + "/" + p
	default:
		return fmt.Errorf("unsupported URL scheme %q (use https, ssh or a local path)", safe.Term(u.Scheme))
	}
	// Web URLs of GitLab sub-pages: .../project/-/tree/main
	if i := strings.Index(p, "/-/"); i >= 0 {
		p = p[:i]
	}
	s.Path = p
	if s.URL == "" {
		u.Scheme = s.Scheme
		u.Host = s.Host
		u.Path = "/" + p
		s.URL = u.String()
	}
	return nil
}

// finish validates the parsed components and settles provider and type.
func (s *Spec) finish() error {
	if err := safe.Host(s.Host); err != nil {
		return err
	}
	s.Path = strings.TrimSuffix(s.Path, ".git")
	if err := safe.RelPath(s.Path); err != nil {
		return fmt.Errorf("source path %q: %w", safe.Term(s.Path), err)
	}
	if s.Provider == "" {
		s.Provider = DetectProvider(s.Host)
	}
	if s.Scheme == "ssh" || s.Scheme == "git" || s.Scheme == "file" {
		// Only repositories can be addressed through git transports;
		// keep the detected forge (for metadata) when it is known.
		if s.Provider == "" {
			s.Provider = Git
		}
		s.Type = TypeRepo
		if s.Provider != Git {
			s.Notes = append(s.Notes, "repository metadata is looked up through the "+s.Provider+" API")
		}
		return nil
	}
	n := strings.Count(s.Path, "/") + 1
	switch s.Provider {
	case "":
		if n < 2 {
			return fmt.Errorf("%s is not a known forge; to archive a whole owner there, pass --provider (github, gitlab or gitea)", s.Host)
		}
		s.Provider, s.Type = Git, TypeRepo
	case Git:
		s.Type = TypeRepo
	case GitHub, Gitea:
		switch n {
		case 1:
			s.Type = TypeOwner
		case 2:
			s.Type = TypeRepo
		default:
			return fmt.Errorf("%q is not an owner or repository URL (expected %s/OWNER or %s/OWNER/REPO)", safe.Term(s.Path), s.Host, s.Host)
		}
	case GitLab:
		s.Type = "" // groups nest arbitrarily; ask the API
	}
	if s.Provider != Git {
		s.URL = s.Scheme + "://" + s.Host + "/" + s.Path
	}
	return nil
}
