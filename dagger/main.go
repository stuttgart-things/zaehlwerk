// Dagger CI module for zaehlwerk.
//
// Lint, build and the image go through the reusable stuttgart-things/dagger/go
// module, the way every Go service here does. Two functions are this project's
// own, and both exist because a scorekeeper that only passes its unit tests has
// not been checked where it actually breaks:
//
//   - Test binds a redis-stack, so the panel's integration tests run instead of
//     skipping themselves. They are the ones that would notice a change in what
//     reaches the stream.
//   - BuildAndTestBinary plays a point through the running binary — a real
//     listener, a real Redis, and the match reading back what was scored on it —
//     which is the path no unit test covers end to end.
//
// The image is scanned by the reusable call-dagger-trivy-image-scan workflow
// rather than from in here: it pins its own Trivy module, and a scan is the
// same call for every project.
package main

import (
	"context"
	"dagger/dagger/internal/dagger"
	"fmt"
)

// The binary is under cmd/, so nothing here can take the default "." that the
// reusable module assumes for a single-package repository.
const (
	mainPackage = "./cmd/zaehlwerk-api"
	binName     = "zaehlwerk-api"

	// goVersionDefault tracks go.mod. It is a floor rather than a pin —
	// GOTOOLCHAIN is auto in these images — but govulncheck and the race
	// detector both behave better when it is the version being shipped.
	goVersionDefault = "1.26.6"

	// redisVersion is a redis-stack, not a plain redis: the panel tests use
	// RedisJSON, and homerun's pitcher writes it.
	redisVersion = "7.2.0-v18"
)

type Dagger struct{}

// Lint runs golangci-lint over the source.
func (m *Dagger) Lint(
	ctx context.Context,
	src *dagger.Directory,
	// +optional
	// +default="500s"
	timeout string,
) *dagger.Container {
	return dag.Go().Lint(src, dagger.GoLintOpts{
		Timeout: timeout,
	})
}

// Test runs the Go test suite with the race detector and a redis-stack bound.
//
// The race detector is not optional for this repository: the scorer is called
// from HTTP handlers while observers read it, and every bug worth catching here
// is a data race. REDIS_TEST_ADDR is what `task test:redis` sets locally, and
// without it the panel tests skip themselves silently.
func (m *Dagger) Test(
	ctx context.Context,
	src *dagger.Directory,
	// +optional
	// +default="1.26.6"
	goVersion string,
	// +optional
	// +default="./..."
	testPath string,
) (string, error) {
	redis := dag.Homerun().RedisService(dagger.HomerunRedisServiceOpts{
		Version:  redisVersion,
		Password: "",
	})

	return dag.Container().
		From("golang:"+goVersion).
		WithDirectory("/src", src).
		WithWorkdir("/src").
		WithMountedCache("/go/pkg/mod", dag.CacheVolume("gomod")).
		WithMountedCache("/root/.cache/go-build", dag.CacheVolume("gobuild")).
		WithServiceBinding("redis", redis).
		WithEnvVariable("REDIS_TEST_ADDR", "redis:6379").
		WithExec([]string{"go", "test", testPath, "-race", "-cover"}).
		Stdout(ctx)
}

// Build compiles the binary.
func (m *Dagger) Build(
	ctx context.Context,
	src *dagger.Directory,
	// +optional
	// +default="1.26.6"
	goVersion string,
	// +optional
	// +default="linux"
	os string,
	// +optional
	// +default="amd64"
	arch string,
) *dagger.Directory {
	return dag.Go().BuildBinary(src, dagger.GoBuildBinaryOpts{
		GoVersion:  goVersion,
		Os:         os,
		Arch:       arch,
		BinName:    binName,
		GoMainFile: mainPackage,
	})
}

// BuildImage builds the container image with ko, and pushes it when asked to.
//
// The build path is passed explicitly as well as being in .ko.yaml, because ko
// takes it as an argument and the config only decides how it is built.
func (m *Dagger) BuildImage(
	ctx context.Context,
	src *dagger.Directory,
	// +optional
	// +default="ko.local/zaehlwerk"
	repo string,
	// +optional
	// +default="false"
	push string,
) (string, error) {
	return dag.Go().KoBuild(ctx, src, dagger.GoKoBuildOpts{
		Repo:     repo,
		BuildArg: mainPackage,
		Push:     push,
	})
}

// BuildAndTestBinary starts the built binary against a redis-stack and plays a
// point through it, returning the log either way.
//
// It covers the seam the unit tests cannot: a real listener, a real Redis, and
// the match reading back the point a POST just put on it. A failure here is a
// wiring failure — a route that moved, a handler that no longer answers — which
// is exactly what a green unit suite hides.
func (m *Dagger) BuildAndTestBinary(
	ctx context.Context,
	source *dagger.Directory,
	// +optional
	// +default="1.26.6"
	goVersion string,
	// +optional
	// +default=8080
	port int,
) (*dagger.File, error) {
	binDir := dag.Go().BuildBinary(source, dagger.GoBuildBinaryOpts{
		GoVersion:  goVersion,
		Os:         "linux",
		Arch:       "amd64",
		BinName:    binName,
		GoMainFile: mainPackage,
	})

	redis := dag.Homerun().RedisService(dagger.HomerunRedisServiceOpts{
		Version:  redisVersion,
		Password: "",
	})

	// sed rather than jq: the image is alpine + curl, and one field out of a
	// known response does not need a JSON parser installed for it.
	script := fmt.Sprintf(`
exec > /app/test-output.log 2>&1
set -e

echo "=== Starting zaehlwerk-api ==="
./%[1]s &
BIN_PID=$!
sleep 3

echo ""
echo "=== Health ==="
curl -fsS http://localhost:%[2]d/healthz

echo ""
echo "=== Creating a match ==="
MATCH=$(curl -fsS -X POST http://localhost:%[2]d/matches \
  -H 'content-type: application/json' \
  -d '{"players":["Anna","Bernd"],"best_of":3}' \
  | sed -n 's/.*"match_id":"\([^"]*\)".*/\1/p')

if [ -z "$MATCH" ]; then
  echo "no match id in the response"
  kill $BIN_PID 2>/dev/null || true
  exit 1
fi
echo "match $MATCH"

echo ""
echo "=== Scoring a point ==="
curl -fsS -X POST http://localhost:%[2]d/ingest/web \
  -H 'content-type: application/json' \
  -d "{\"match_id\":\"$MATCH\",\"source\":\"dagger-smoke\",\"player\":\"a\",\"delta\":1,\"event_id\":1}"

echo ""
echo "=== The match carries it ==="
curl -fsS "http://localhost:%[2]d/matches/$MATCH" | grep -q '"points":\[1,0\]' || {
  echo "the match does not show the point that was just scored"
  kill $BIN_PID 2>/dev/null || true
  exit 1
}

echo ""
echo "=== All checks passed ==="
kill $BIN_PID 2>/dev/null || true
exit 0
`, binName, port)

	result := dag.Container().
		From("alpine:latest").
		WithExec([]string{"apk", "add", "--no-cache", "curl"}).
		WithDirectory("/app", binDir).
		WithWorkdir("/app").
		WithServiceBinding("redis", redis).
		// With REDIS_ADDR set, the panel sink pitches every transition onto the
		// stream, so the smoke test exercises that path rather than the one
		// where the panel is quietly disabled.
		WithEnvVariable("REDIS_ADDR", "redis").
		WithEnvVariable("REDIS_PORT", "6379").
		WithEnvVariable("PANEL_STREAM", "tabletennis").
		WithExec([]string{"sh", "-c", script})

	log := result.File("/app/test-output.log")
	if _, err := result.Sync(ctx); err != nil {
		return log, fmt.Errorf("the smoke test failed, see test-output.log: %w", err)
	}
	return log, nil
}
