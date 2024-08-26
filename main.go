package main

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/google/go-github/v39/github"
	"github.com/kelseyhightower/envconfig"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"golang.org/x/oauth2"
)

//go:embed templates
var templates embed.FS

type Config struct {
	Port            int           `envconfig:"PORT" default:"8080"`
	GitHubToken     string        `envconfig:"GITHUB_TOKEN"`
	Repos           []string      `envconfig:"REPOS"`
	RefreshInterval time.Duration `envconfig:"REFRESH_INTERVAL" default:"1h"`
}

type Releases struct {
	Releases     []Release
	FetchedAt    time.Time
	FetchedAtAgo string
}

type Release struct {
	Repo              Repo
	LatestTag         string
	LatestTagAgo      string
	UnreleasedCommits int
}

type Repo struct {
	FullName string
	Owner    string
	Name     string
	Branch   string
}

func NewRepoFromString(repo string) (Repo, error) {
	parts := strings.Split(repo, ":")
	repoParts := strings.Split(parts[0], "/")

	if len(repoParts) != 2 {
		return Repo{}, fmt.Errorf("invalid repo format: %s", repo)
	}

	branch := "main"
	if len(parts) > 1 {
		branch = parts[1]
	}

	return Repo{
		FullName: parts[0],
		Owner:    repoParts[0],
		Name:     repoParts[1],
		Branch:   branch,
	}, nil
}

type Cache struct {
	releases Releases
	lock     sync.RWMutex
	cfg      Config
	client   *github.Client
}

func NewCache(cfg Config) *Cache {
	var client *http.Client
	if cfg.GitHubToken == "" {
		client = http.DefaultClient
	} else {
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: cfg.GitHubToken},
		)
		client = oauth2.NewClient(context.Background(), ts)
	}
	gh := github.NewClient(client)

	return &Cache{
		cfg:    cfg,
		client: gh,
	}
}

func (c *Cache) Get() Releases {
	c.lock.RLock()
	defer c.lock.RUnlock()
	return c.releases
}

func (c *Cache) Refresh(ctx context.Context) {
	c.lock.Lock()
	defer c.lock.Unlock()

	slog.Info("Refreshing cache...")

	type result struct {
		release Release
		err     error
	}

	ch := make(chan result)

	for _, repoName := range c.cfg.Repos {
		go func() {
			release, err := c.fetchRelease(ctx, repoName)
			if err != nil {
				ch <- result{err: err}
				return
			}

			ch <- result{release: release}
		}()
	}

	var releases []Release
	for range len(c.cfg.Repos) {
		select {
		case r := <-ch:
			if r.err != nil {
				slog.Error("Error fetching release:", r.err)
				continue
			}

			releases = append(releases, r.release)
		case <-ctx.Done():
			slog.Warn("Cache refresh cancelled")
		}
	}

	c.releases = Releases{
		Releases:  releases,
		FetchedAt: time.Now(),
	}

	slog.Info("Cache refreshed")
}

func (c *Cache) fetchRelease(ctx context.Context, repoName string) (Release, error) {
	repo, err := NewRepoFromString(repoName)
	if err != nil {
		return Release{}, err
	}

	latestRelease, _, err := c.client.Repositories.GetLatestRelease(ctx, repo.Owner, repo.Name)
	if err != nil {
		return Release{}, fmt.Errorf("error fetching latest release for %s: %w", repoName, err)
	}

	comparison, _, err := c.client.Repositories.CompareCommits(ctx, repo.Owner, repo.Name, *latestRelease.TagName, repo.Branch, nil)
	if err != nil {
		return Release{}, fmt.Errorf("error comparing commits for %s: %w", repoName, err)
	}

	publishedAt := latestRelease.GetPublishedAt()
	latestTagAgo := humanize.Time(publishedAt.Time)

	return Release{
		Repo:              repo,
		LatestTag:         *latestRelease.TagName,
		LatestTagAgo:      latestTagAgo,
		UnreleasedCommits: *comparison.TotalCommits,
	}, nil
}

func main() {
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		panic(err)
	}

	cache := NewCache(cfg)

	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
			cache.Refresh(ctx)
			cancel()
			time.Sleep(cfg.RefreshInterval)
		}
	}()

	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())

	e.GET("/", func(c echo.Context) error {
		releases := cache.Get()
		releases.FetchedAtAgo = humanize.Time(releases.FetchedAt)
		return c.Render(http.StatusOK, "index.html", releases)
	})

	e.Renderer = &TemplateRenderer{
		templates: template.Must(template.ParseFS(templates, "templates/index.html")),
	}

	err := e.Start(fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		panic(err)
	}
}

type TemplateRenderer struct {
	templates *template.Template
}

func (t *TemplateRenderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return t.templates.ExecuteTemplate(w, name, data)
}
