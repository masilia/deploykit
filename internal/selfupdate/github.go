package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Where the releases are. A release has an archive per platform (deploykit_<version>_<os>_<arch>.tar.gz) and a checksums.txt.
const (
	DefaultAPI  = "https://api.github.com"
	DefaultRepo = "masilia/deploykit"
)

// Limits on what is read from the network: a release is a few megabytes, so anything far beyond that is refused.
var (
	maxJSON      int64 = 4 << 20
	maxChecksums int64 = 1 << 20
	maxArchive   int64 = 150 << 20
)

// Release is a published version of the project.
type Release struct {
	Tag        string
	Version    Version
	Prerelease bool
	Assets     map[string]string // file name -> download URL
}

// Client talks to the GitHub releases API of one repository. The API is public: no token is sent or needed.
type Client struct {
	API       string       // DefaultAPI
	Repo      string       // DefaultRepo
	HTTP      *http.Client // a timeout is set by DefaultClient
	UserAgent string
	// AllowHTTP lets the URLs be plain http. Production never sets it: a download over http could be replaced on the way.
	// Tests use it, since their server is not TLS.
	AllowHTTP bool
}

// DefaultClient is the client of the project's public releases.
func DefaultClient(userAgent string) *Client {
	return &Client{API: DefaultAPI, Repo: DefaultRepo, HTTP: &http.Client{Timeout: 60 * time.Second}, UserAgent: userAgent}
}

type apiRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Releases lists the published releases (drafts are not visible without a token, and are dropped anyway), whose tag is a version.
func (c *Client) Releases(ctx context.Context) ([]Release, error) {
	u := strings.TrimRight(c.API, "/") + "/repos/" + c.Repo + "/releases?per_page=50"
	body, err := c.get(ctx, u, maxJSON, "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("list the releases of %s: %w", c.Repo, err)
	}
	var raw []apiRelease
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("list the releases of %s: unreadable answer: %w", c.Repo, err)
	}
	var out []Release
	for _, r := range raw {
		v, err := Parse(r.Tag)
		if err != nil || r.Draft {
			continue
		}
		rel := Release{Tag: r.Tag, Version: v, Prerelease: r.Prerelease || v.Prerelease(), Assets: map[string]string{}}
		for _, a := range r.Assets {
			rel.Assets[a.Name] = a.URL
		}
		out = append(out, rel)
	}
	return out, nil
}

// Latest is the newest release by version number (not by the order of the answer); pre-releases only when pre is set.
func (c *Client) Latest(ctx context.Context, pre bool) (Release, error) {
	all, err := c.Releases(ctx)
	if err != nil {
		return Release{}, err
	}
	var best *Release
	for i := range all {
		r := &all[i]
		if r.Prerelease && !pre {
			continue
		}
		if best == nil || r.Version.Compare(best.Version) > 0 {
			best = r
		}
	}
	if best == nil {
		return Release{}, errors.New("no published release found")
	}
	return *best, nil
}

// Find is the release of one version (X.Y.Z, with or without the v).
func (c *Client) Find(ctx context.Context, version string) (Release, error) {
	want, err := Parse(version)
	if err != nil {
		return Release{}, err
	}
	all, err := c.Releases(ctx)
	if err != nil {
		return Release{}, err
	}
	for _, r := range all {
		if r.Version.Compare(want) == 0 {
			return r, nil
		}
	}
	return Release{}, fmt.Errorf("no published release %s", want)
}

// ArchiveName is the file name of the archive of a release for a platform, as GoReleaser names it.
func ArchiveName(v Version, goos, goarch string) string {
	return fmt.Sprintf("deploykit_%s_%s_%s.tar.gz", v, goos, goarch)
}

// get reads a URL, at most limit bytes. Only https is accepted unless AllowHTTP is set.
func (c *Client) get(ctx context.Context, rawURL string, limit int64, accept string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("bad URL %q", rawURL)
	}
	if u.Scheme != "https" && (!c.AllowHTTP || u.Scheme != "http") {
		return nil, fmt.Errorf("refusing %s: only https downloads are accepted", u.Redacted())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", u.Host, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("the answer of %s is larger than %d bytes: refused", u.Host, limit)
	}
	return data, nil
}
