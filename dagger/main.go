// Dagger CI/CD module for llm-proxy-manifestd.
package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"dagger/llm-proxy-manifestd/internal/dagger"
)

const (
	registry  = "ghcr.io"
	imageRepo = "ghcr.io/lerenn/llm-proxy-manifestd"
	// Adapter images: ghcr.io/lerenn/llm-proxy-manifestd-adapter-<name>.
	adapterImagePrefix = "ghcr.io/lerenn/llm-proxy-manifestd-adapter-"
	chartRepo          = "oci://ghcr.io/lerenn/charts"
	chartPath          = "deploy/charts/llm-proxy-manifestd"
	goImage            = "golang:1.23"
	helmImage          = "alpine/helm:3.16.4"
	releaseTag         = `^v?[0-9]+\.[0-9]+\.[0-9]+$`
)

// LlmProxyManifestd is the module entrypoint.
type LlmProxyManifestd struct{}

// Test runs the Go test suite with the race detector.
func (m *LlmProxyManifestd) Test(
	ctx context.Context,
	// +defaultPath="/"
	// +ignore=[".git", "website/node_modules", "website/.vitepress/dist", "website/.vitepress/cache"]
	source *dagger.Directory,
) (string, error) {
	return dag.Container().
		From(goImage).
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("llm-proxy-manifestd-gomod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("llm-proxy-manifestd-gobuild")).
		WithDirectory("/src", source).
		WithWorkdir("/src").
		WithExec([]string{"go", "test", "-race", "./..."}).
		Stdout(ctx)
}

// PublishImage builds a multi-arch (linux/amd64, linux/arm64) image and pushes
// it to ghcr.io/lerenn/llm-proxy-manifestd:<tag>. Release tags (vX.Y.Z) are
// also published as the bare version and as :latest.
func (m *LlmProxyManifestd) PublishImage(
	ctx context.Context,
	// +defaultPath="/"
	// +ignore=[".git", "website/node_modules", "website/.vitepress/dist", "website/.vitepress/cache"]
	source *dagger.Directory,
	// Image tag, e.g. v0.1.0
	tag string,
	// Registry username
	registryUser string,
	// Registry token or password
	registryToken *dagger.Secret,
) (string, error) {
	version := strings.TrimPrefix(tag, "v")
	var variants []*dagger.Container
	for _, platform := range []dagger.Platform{"linux/amd64", "linux/arm64"} {
		variants = append(variants, source.DockerBuild(dagger.DirectoryDockerBuildOpts{
			Platform:  platform,
			BuildArgs: []dagger.BuildArg{{Name: "VERSION", Value: version}},
		}))
	}

	tags := []string{tag}
	if regexp.MustCompile(releaseTag).MatchString(tag) {
		if version != tag {
			tags = append(tags, version)
		}
		tags = append(tags, "latest")
	}

	publisher := dag.Container().WithRegistryAuth(registry, registryUser, registryToken)
	var refs []string
	for _, t := range tags {
		ref, err := publisher.Publish(ctx, fmt.Sprintf("%s:%s", imageRepo, t),
			dagger.ContainerPublishOpts{PlatformVariants: variants})
		if err != nil {
			return "", fmt.Errorf("publish %s: %w", t, err)
		}
		refs = append(refs, ref)
	}
	return strings.Join(refs, "\n"), nil
}

// PublishChart packages the Helm chart and pushes it to
// oci://ghcr.io/lerenn/charts.
func (m *LlmProxyManifestd) PublishChart(
	ctx context.Context,
	// +defaultPath="/"
	// +ignore=[".git", "website/node_modules", "website/.vitepress/dist", "website/.vitepress/cache"]
	source *dagger.Directory,
	// Chart version, e.g. v0.1.0 (a leading "v" is stripped; also used as appVersion)
	version string,
	// Registry username
	registryUser string,
	// Registry token or password
	registryToken *dagger.Secret,
) (string, error) {
	v := strings.TrimPrefix(version, "v")
	return dag.Container().
		From(helmImage).
		WithDirectory("/chart", source.Directory(chartPath)).
		WithSecretVariable("REGISTRY_TOKEN", registryToken).
		WithEnvVariable("REGISTRY_USER", registryUser).
		WithEnvVariable("CHART_VERSION", v).
		WithEnvVariable("CHART_REPO", chartRepo).
		WithExec([]string{"sh", "-c", strings.Join([]string{
			"set -e",
			`helm package /chart --version "$CHART_VERSION" --app-version "$CHART_VERSION" -d /out`,
			`echo "$REGISTRY_TOKEN" | helm registry login ` + registry + ` -u "$REGISTRY_USER" --password-stdin`,
			`helm push /out/llm-proxy-manifestd-"$CHART_VERSION".tgz "$CHART_REPO"`,
		}, "\n")}).
		Stdout(ctx)
}

// adapterImage builds the adapter image for one platform. name is the adapter
// directory under cmd/ (e.g. "codex").
func adapterImage(source *dagger.Directory, name string, platform dagger.Platform) *dagger.Container {
	return source.DockerBuild(dagger.DirectoryDockerBuildOpts{
		Platform:   platform,
		Dockerfile: fmt.Sprintf("cmd/%s/Dockerfile", name),
	})
}

// BuildAdapter builds the adapter image (linux/amd64) without publishing it,
// to catch Dockerfile and binary-download breakage in CI. It also runs
// `<name> --version` (the vendor CLI) inside the image.
func (m *LlmProxyManifestd) BuildAdapter(
	ctx context.Context,
	// +defaultPath="/"
	// +ignore=[".git", "website/node_modules", "website/.vitepress/dist", "website/.vitepress/cache"]
	source *dagger.Directory,
	// Adapter name (directory under cmd/)
	// +default="codex"
	name string,
) (string, error) {
	return adapterImage(source, name, "linux/amd64").
		WithEntrypoint([]string{"/usr/local/bin/" + name}).
		WithExec([]string{"--version"}, dagger.ContainerWithExecOpts{UseEntrypoint: true}).
		Stdout(ctx)
}

// PublishAdapterImage builds a multi-arch adapter image and pushes it to
// ghcr.io/lerenn/llm-proxy-manifestd-adapter-<name>:<tag>, with the same
// tagging rules as PublishImage.
func (m *LlmProxyManifestd) PublishAdapterImage(
	ctx context.Context,
	// +defaultPath="/"
	// +ignore=[".git", "website/node_modules", "website/.vitepress/dist", "website/.vitepress/cache"]
	source *dagger.Directory,
	// Adapter name (directory under cmd/)
	// +default="codex"
	name string,
	// Image tag, e.g. v0.1.0
	tag string,
	// Registry username
	registryUser string,
	// Registry token or password
	registryToken *dagger.Secret,
) (string, error) {
	version := strings.TrimPrefix(tag, "v")
	var variants []*dagger.Container
	for _, platform := range []dagger.Platform{"linux/amd64", "linux/arm64"} {
		variants = append(variants, adapterImage(source, name, platform))
	}
	tags := []string{tag}
	if regexp.MustCompile(releaseTag).MatchString(tag) {
		if version != tag {
			tags = append(tags, version)
		}
		tags = append(tags, "latest")
	}
	publisher := dag.Container().WithRegistryAuth(registry, registryUser, registryToken)
	var refs []string
	for _, t := range tags {
		ref, err := publisher.Publish(ctx, fmt.Sprintf("%s%s:%s", adapterImagePrefix, name, t),
			dagger.ContainerPublishOpts{PlatformVariants: variants})
		if err != nil {
			return "", fmt.Errorf("publish %s: %w", t, err)
		}
		refs = append(refs, ref)
	}
	return strings.Join(refs, "\n"), nil
}

// cliPlatforms are the lpmctl release targets (GOOS/GOARCH).
var cliPlatforms = []struct{ os, arch string }{
	{"linux", "amd64"}, {"linux", "arm64"},
	{"darwin", "amd64"}, {"darwin", "arm64"},
}

// BuildCli cross-compiles the lpmctl CLI (static, CGO disabled) for
// linux/darwin x amd64/arm64 and returns a directory with one binary per
// target, named lpmctl-<os>-<arch>, plus lpmctl-sha256sums.txt. These are
// the GitHub release assets; lpmctl is not shipped in any image.
func (m *LlmProxyManifestd) BuildCli(
	// +defaultPath="/"
	// +ignore=[".git", "website/node_modules", "website/.vitepress/dist", "website/.vitepress/cache"]
	source *dagger.Directory,
	// Version embedded in `lpmctl --version`, e.g. v0.1.0
	// +default="dev"
	version string,
) *dagger.Directory {
	ctr := dag.Container().
		From(goImage).
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("llm-proxy-manifestd-gomod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("llm-proxy-manifestd-gobuild")).
		WithDirectory("/src", source).
		WithWorkdir("/src").
		WithEnvVariable("CGO_ENABLED", "0").
		WithExec([]string{"mkdir", "-p", "/dist"})
	var names []string
	for _, p := range cliPlatforms {
		name := fmt.Sprintf("lpmctl-%s-%s", p.os, p.arch)
		names = append(names, name)
		ctr = ctr.
			WithEnvVariable("GOOS", p.os).
			WithEnvVariable("GOARCH", p.arch).
			WithExec([]string{"go", "build", "-trimpath",
				"-ldflags", "-s -w -X main.version=" + version,
				"-o", "/dist/" + name, "./cmd/cli"})
	}
	ctr = ctr.
		WithWorkdir("/dist").
		WithExec([]string{"sh", "-c", "sha256sum " + strings.Join(names, " ") + " > lpmctl-sha256sums.txt"})
	return ctr.Directory("/dist")
}
