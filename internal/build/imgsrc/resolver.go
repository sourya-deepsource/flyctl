package imgsrc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/vektah/gqlparser/v2/gqlerror"
	"go.opentelemetry.io/otel/attribute"

	dockerclient "github.com/docker/docker/client"
	"github.com/superfly/flyctl/client"
	"github.com/superfly/flyctl/gql"
	"github.com/superfly/flyctl/internal/buildinfo"
	"github.com/superfly/flyctl/internal/config"
	"github.com/superfly/flyctl/internal/sentry"
	"github.com/superfly/flyctl/internal/tracing"
	"github.com/superfly/flyctl/iostreams"

	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/terminal"
)

type ImageOptions struct {
	AppName              string
	WorkingDir           string
	DockerfilePath       string
	IgnorefilePath       string
	ImageRef             string
	BuildArgs            map[string]string
	ExtraBuildArgs       map[string]string
	BuildSecrets         map[string]string
	ImageLabel           string
	Publish              bool
	Tag                  string
	Target               string
	NoCache              bool
	BuiltIn              string
	BuiltInSettings      map[string]interface{}
	Builder              string
	Buildpacks           []string
	Label                map[string]string
	BuildpacksDockerHost string
	BuildpacksVolumes    []string
}

func (io ImageOptions) ToSpanAttributes() []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("imageoptions.app_name", io.AppName),
		attribute.String("imageoptions.work_dir", io.WorkingDir),
		attribute.String("imageoptions.dockerfile_path", io.DockerfilePath),
		attribute.String("imageoptions.ignorefile_path", io.IgnorefilePath),
		attribute.String("imageoptions.image.ref", io.ImageRef),
		attribute.String("imageoptions.image.label", io.ImageLabel),
		attribute.Bool("imageoptions.publish", io.Publish),
		attribute.String("imageoptions.tag", io.Tag),
		attribute.Bool("imageoptions.nocache", io.NoCache),
		attribute.String("imageoptions.builtin", io.BuiltIn),
		attribute.String("imageoptions.builder", io.BuiltIn),
		attribute.StringSlice("imageoptions.buildpacks", io.Buildpacks),
	}

	b, err := json.Marshal(io.BuildArgs)
	if err == nil {
		attrs = append(attrs, attribute.String("imageoptions.build_args", string(b)))
	}

	b, err = json.Marshal(io.ExtraBuildArgs)
	if err == nil {
		attrs = append(attrs, attribute.String("imageoptions.extra_build_args", string(b)))
	}

	b, err = json.Marshal(io.BuildSecrets)
	if err == nil {
		attrs = append(attrs, attribute.String("imageoptions.build_secrets", string(b)))
	}

	b, err = json.Marshal(io.BuiltInSettings)
	if err == nil {
		attrs = append(attrs, attribute.String("imageoptions.built_in_settings", string(b)))
	}

	b, err = json.Marshal(io.Label)
	if err == nil {
		attrs = append(attrs, attribute.String("imageoptions.labels", string(b)))
	}

	return attrs

}

type RefOptions struct {
	AppName    string
	WorkingDir string
	ImageRef   string
	ImageLabel string
	Publish    bool
	Tag        string
}

func (ro RefOptions) ToSpanAttributes() []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("refoptions.app_name", ro.AppName),
		attribute.String("refoptions.work_dir", ro.WorkingDir),
		attribute.String("refoptions.image.ref", ro.ImageRef),
		attribute.String("refoptions.image.label", ro.ImageLabel),
		attribute.Bool("refoptions.publish", ro.Publish),
		attribute.String("refoptions.tag", ro.Tag),
	}
}

type DeploymentImage struct {
	ID     string
	Tag    string
	Size   int64
	Labels map[string]string
}

func (di DeploymentImage) ToSpanAttributes() []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("image.id", di.ID),
		attribute.String("image.tag", di.Tag),
		attribute.Int64("image.size", di.Size),
	}

	b, err := json.Marshal(di.Labels)
	if err == nil {
		attrs = append(attrs, attribute.String("image.labels", string(b)))
	}

	return attrs
}

type Resolver struct {
	dockerFactory *dockerClientFactory
	apiClient     *api.Client
}

type StopSignal struct {
	Chan chan struct{}
	once sync.Once
}

// limit stored logs to 4KB; take suffix if longer
const logLimit int = 4096

// ResolveReference returns an Image give an reference using either the local docker daemon or remote registry
func (r *Resolver) ResolveReference(ctx context.Context, streams *iostreams.IOStreams, opts RefOptions) (img *DeploymentImage, err error) {
	ctx, span := tracing.GetTracer().Start(ctx, "resolve_reference")
	defer span.End()

	strategies := []imageResolver{
		&localImageResolver{},
		&remoteImageResolver{flyApi: r.apiClient},
	}

	bld, err := r.createImageBuild(ctx, strategies, opts)
	if err != nil {
		span.AddEvent(fmt.Sprintf("failed to create image build. err=%s", err.Error()))
		terminal.Warnf("failed to create build in graphql: %v\n", err)
	}

	for _, s := range strategies {
		terminal.Debugf("Trying '%s' strategy\n", s.Name())
		bld.ResetTimings()
		bld.BuildAndPushStart()
		var note string
		img, note, err = s.Run(ctx, r.dockerFactory, streams, opts, bld)
		terminal.Debugf("result image:%+v error:%v\n", img, err)
		if err != nil {
			bld.BuildAndPushFinish()
			bld.FinishImageStrategy(s, true /* failed */, err, note)
			r.finishBuild(ctx, bld, true /* failed */, err.Error(), nil)
			return nil, err
		}
		if img != nil {
			bld.BuildAndPushFinish()
			bld.FinishImageStrategy(s, false /* success */, nil, note)
			r.finishBuild(ctx, bld, false /* completed */, "", img)
			return img, nil
		}
		bld.BuildAndPushFinish()
		bld.FinishImageStrategy(s, true /* failed */, nil, note)
		span.AddEvent(fmt.Sprintf("failed to resolve image with strategy %s", s.Name()))

	}

	r.finishBuild(ctx, bld, true /* failed */, "no strategies resulted in an image", nil)
	err = fmt.Errorf("could not find image %q", opts.ImageRef)
	tracing.RecordError(span, err, "failed to resolve image")
	return nil, err
}

// BuildImage converts source code to an image using a Dockerfile, buildpacks, or builtins.
func (r *Resolver) BuildImage(ctx context.Context, streams *iostreams.IOStreams, opts ImageOptions) (img *DeploymentImage, err error) {
	ctx, span := tracing.GetTracer().Start(ctx, "build_image")
	defer span.End()

	if !r.dockerFactory.mode.IsAvailable() {
		err := errors.New("docker is unavailable to build the deployment image")
		tracing.RecordError(span, err, "docker is unavailable to build the deployment image")
		return nil, err
	}

	if opts.Tag == "" {
		opts.Tag = NewDeploymentTag(opts.AppName, opts.ImageLabel)
	}

	span.SetAttributes(attribute.String("tag", opts.Tag))

	strategies := []imageBuilder{}

	if r.dockerFactory.mode.UseNixpacks() {
		strategies = append(strategies, &nixpacksBuilder{})
	} else {
		strategies = []imageBuilder{
			&buildpacksBuilder{},
			&dockerfileBuilder{},
			&builtinBuilder{},
		}
	}

	strategiesString := []string{}
	for _, strategy := range strategies {
		strategiesString = append(strategiesString, strategy.Name())
	}

	span.SetAttributes(attribute.String("strategies", strings.Join(strategiesString, ",")))

	bld, err := r.createBuild(ctx, strategies, opts)
	if err != nil {
		terminal.Warnf("failed to create build in graphql: %v\n", err)
	}
	for _, s := range strategies {
		terminal.Debugf("Trying '%s' strategy\n", s.Name())
		bld.ResetTimings()
		bld.BuildAndPushStart()
		var note string
		img, note, err = s.Run(ctx, r.dockerFactory, streams, opts, bld)
		terminal.Debugf("result image:%+v error:%v\n", img, err)
		if err != nil {
			bld.BuildAndPushFinish()
			bld.FinishStrategy(s, true /* failed */, err, note)
			r.finishBuild(ctx, bld, true /* failed */, err.Error(), nil)
			return nil, err
		}
		if img != nil {
			bld.BuildAndPushFinish()
			bld.FinishStrategy(s, false /* success */, nil, note)
			r.finishBuild(ctx, bld, false /* completed */, "", img)
			return img, nil
		}
		bld.BuildAndPushFinish()
		bld.FinishStrategy(s, true /* failed */, nil, note)
	}

	r.finishBuild(ctx, bld, true /* failed */, "no strategies resulted in an image", nil)
	return nil, errors.New("app does not have a Dockerfile or buildpacks configured. See https://fly.io/docs/reference/configuration/#the-build-section")
}

func (r *Resolver) createImageBuild(ctx context.Context, strategies []imageResolver, opts RefOptions) (*build, error) {
	strategiesAvailable := make([]string, 0)
	for _, r := range strategies {
		strategiesAvailable = append(strategiesAvailable, r.Name())
	}
	imageOps := &gql.BuildImageOptsInput{
		ImageLabel: opts.ImageLabel,
		ImageRef:   opts.ImageRef,
		Publish:    opts.Publish,
		Tag:        opts.Tag,
	}
	return r.createBuildGql(ctx, strategiesAvailable, imageOps)
}

func (r *Resolver) createBuild(ctx context.Context, strategies []imageBuilder, opts ImageOptions) (*build, error) {
	strategiesAvailable := make([]string, 0)
	for _, s := range strategies {
		strategiesAvailable = append(strategiesAvailable, s.Name())
	}
	imageOpts := &gql.BuildImageOptsInput{
		BuildArgs:       opts.BuildArgs,
		BuildPacks:      opts.Buildpacks,
		Builder:         opts.Builder,
		BuiltIn:         opts.BuiltIn,
		BuiltInSettings: opts.BuiltInSettings,
		DockerfilePath:  opts.DockerfilePath,
		ExtraBuildArgs:  opts.ExtraBuildArgs,
		ImageLabel:      opts.ImageLabel,
		ImageRef:        opts.ImageRef,
		NoCache:         opts.NoCache,
		Publish:         opts.Publish,
		Tag:             opts.Tag,
		Target:          opts.Target,
	}
	return r.createBuildGql(ctx, strategiesAvailable, imageOpts)
}

func (r *Resolver) createBuildGql(ctx context.Context, strategiesAvailable []string, imageOpts *gql.BuildImageOptsInput) (*build, error) {
	ctx, span := tracing.GetTracer().Start(ctx, "web.create_build")
	defer span.End()

	gqlClient := client.FromContext(ctx).API().GenqClient
	_ = `# @genqlient
	mutation ResolverCreateBuild($input:CreateBuildInput!) {
		createBuild(input:$input) {
			id
			status
		}
	}
	`
	builderType := "local"
	if r.dockerFactory.remote {
		builderType = "remote"
	}
	input := gql.CreateBuildInput{
		AppName:             r.dockerFactory.appName,
		BuilderType:         builderType,
		ImageOpts:           *imageOpts,
		MachineId:           "",
		StrategiesAvailable: strategiesAvailable,
	}
	resp, err := gql.ResolverCreateBuild(ctx, gqlClient, input)
	if err != nil {
		var gqlErr *gqlerror.Error
		isAppNotFoundErr := errors.As(err, &gqlErr) && gqlErr.Path.String() == "createBuild" && gqlErr.Message == "Could not find App"
		if !isAppNotFoundErr {
			sentry.CaptureException(err,
				sentry.WithTraceID(ctx),
				sentry.WithTag("feature", "build-api-create-build"),
				sentry.WithContexts(map[string]sentry.Context{
					"app": map[string]interface{}{
						"name": input.AppName,
					},
					"builder": map[string]interface{}{
						"type": input.BuilderType,
					},
				}),
			)
		}
		span.SetAttributes(attribute.Bool("is_app_not_found_error", isAppNotFoundErr))
		tracing.RecordError(span, err, "failed to create build")
		return newFailedBuild(), err
	}

	b := newBuild(resp.CreateBuild.Id, false)
	// TODO: maybe try to capture SIGINT, SIGTERM and send r.FinishBuild(). I tried, and it usually segfaulted. (tvd, 2022-10-14)
	return b, nil
}

func limitLogs(log string) string {
	if len(log) > logLimit {
		return log[len(log)-logLimit:]
	}
	return log
}

type build struct {
	CreateApiFailed bool
	BuildId         string
	BuilderMeta     *gql.BuilderMetaInput
	StrategyResults []gql.BuildStrategyAttemptInput
	Timings         *gql.BuildTimingsInput
	StartTimes      *gql.BuildTimingsInput
}

func newFailedBuild() *build {
	return newBuild("", true)
}

func newBuild(buildId string, createApiFailed bool) *build {
	return &build{
		CreateApiFailed: createApiFailed,
		BuildId:         buildId,
		BuilderMeta:     &gql.BuilderMetaInput{},
		StrategyResults: make([]gql.BuildStrategyAttemptInput, 0),
		StartTimes:      &gql.BuildTimingsInput{},
		Timings: &gql.BuildTimingsInput{
			BuildAndPushMs: -1,
			BuilderInitMs:  -1,
			BuildMs:        -1,
			ContextBuildMs: -1,
			ImageBuildMs:   -1,
			PushMs:         -1,
		},
	}
}

func (b *build) SetBuilderMetaPart1(remote bool, remoteAppName string, remoteMachineId string) {
	if b == nil {
		return
	}
	builderType := "remote"
	if !remote {
		builderType = "local"
	}
	b.BuilderMeta.BuilderType = builderType
	b.BuilderMeta.RemoteAppName = remoteAppName
	b.BuilderMeta.RemoteMachineId = remoteMachineId
}

func (b *build) SetBuilderMetaPart2(buildkitEnabled bool, dockerVersion string, platform string) {
	b.BuilderMeta.BuildkitEnabled = buildkitEnabled
	b.BuilderMeta.DockerVersion = dockerVersion
	b.BuilderMeta.Platform = platform
}

// call this at the start of each strategy to restart all the timers
func (b *build) ResetTimings() {
	b.StartTimes = &gql.BuildTimingsInput{}
	b.Timings = &gql.BuildTimingsInput{
		BuildAndPushMs: -1,
		BuilderInitMs:  -1,
		BuildMs:        -1,
		ContextBuildMs: -1,
		ImageBuildMs:   -1,
		PushMs:         -1,
	}
}

func (b *build) BuildAndPushStart() {
	b.StartTimes.BuildAndPushMs = time.Now().UnixMilli()
}

func (b *build) BuildAndPushFinish() {
	b.Timings.BuildAndPushMs = time.Now().UnixMilli() - b.StartTimes.BuildAndPushMs
}

func (b *build) BuilderInitStart() {
	b.StartTimes.BuilderInitMs = time.Now().UnixMilli()
}

func (b *build) BuilderInitFinish() {
	b.Timings.BuilderInitMs = time.Now().UnixMilli() - b.StartTimes.BuilderInitMs
}

func (b *build) BuildStart() {
	b.StartTimes.BuildMs = time.Now().UnixMilli()
}

func (b *build) BuildFinish() {
	b.Timings.BuildMs = time.Now().UnixMilli() - b.StartTimes.BuildMs
}

func (b *build) ContextBuildStart() {
	b.StartTimes.ContextBuildMs = time.Now().UnixMilli()
}

func (b *build) ContextBuildFinish() {
	b.Timings.ContextBuildMs = time.Now().UnixMilli() - b.StartTimes.ContextBuildMs
}

func (b *build) ImageBuildStart() {
	b.StartTimes.ImageBuildMs = time.Now().UnixMilli()
}

func (b *build) ImageBuildFinish() {
	b.Timings.ImageBuildMs = time.Now().UnixMilli() - b.StartTimes.ImageBuildMs
}

func (b *build) PushStart() {
	b.StartTimes.PushMs = time.Now().UnixMilli()
}

func (b *build) PushFinish() {
	b.Timings.PushMs = time.Now().UnixMilli() - b.StartTimes.PushMs
}

func (b *build) finishStrategyCommon(strategy string, failed bool, err error, note string) {
	result := "failed"
	if !failed {
		result = "success"
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	r := gql.BuildStrategyAttemptInput{
		Strategy: strategy,
		Result:   result,
		Error:    limitLogs(errMsg),
		Note:     limitLogs(note),
	}
	b.StrategyResults = append(b.StrategyResults, r)
}

func (b *build) FinishStrategy(strategy imageBuilder, failed bool, err error, note string) {
	b.finishStrategyCommon(strategy.Name(), failed, err, note)
}

func (b *build) FinishImageStrategy(strategy imageResolver, failed bool, err error, note string) {
	b.finishStrategyCommon(strategy.Name(), failed, err, note)
}

type buildResult struct {
	BuildId         string
	Status          string
	wallclockTimeMs int
}

func (r *Resolver) finishBuild(ctx context.Context, build *build, failed bool, logs string, img *DeploymentImage) (*buildResult, error) {
	if build.CreateApiFailed {
		terminal.Debug("Skipping FinishBuild() gql call, because CreateBuild() failed.\n")
		return nil, nil
	}
	gqlClient := client.FromContext(ctx).API().GenqClient
	_ = `# @genqlient
	mutation ResolverFinishBuild($input:FinishBuildInput!) {
		finishBuild(input:$input) {
			id
			status
			wallclockTimeMs
		}
	}
	`
	status := "failed"
	if !failed {
		status = "completed"
	}
	input := gql.FinishBuildInput{
		BuildId:             build.BuildId,
		AppName:             r.dockerFactory.appName,
		MachineId:           "",
		Status:              status,
		Logs:                limitLogs(logs),
		BuilderMeta:         *build.BuilderMeta,
		StrategiesAttempted: build.StrategyResults,
		Timings:             *build.Timings,
	}
	if img != nil {
		input.FinalImage = gql.BuildFinalImageInput{
			Id:        img.ID,
			Tag:       img.Tag,
			SizeBytes: img.Size,
		}
	}
	resp, err := gql.ResolverFinishBuild(ctx, gqlClient, input)
	if err != nil {
		terminal.Warnf("failed to finish build in graphql: %v\n", err)
		sentry.CaptureException(err,
			sentry.WithTraceID(ctx),
			sentry.WithTag("feature", "build-api-finish-build"),
			sentry.WithContexts(map[string]sentry.Context{
				"app": map[string]interface{}{
					"name": r.dockerFactory.appName,
				},
				"sourceBuild": map[string]interface{}{
					"id": build.BuildId,
				},
				"builder": map[string]interface{}{
					"type":            build.BuilderMeta.BuilderType,
					"appName":         build.BuilderMeta.RemoteAppName,
					"machineId":       build.BuilderMeta.RemoteMachineId,
					"dockerVersion":   build.BuilderMeta.DockerVersion,
					"buildkitEnabled": build.BuilderMeta.BuildkitEnabled,
				},
			}),
		)
		return nil, err
	}
	return &buildResult{
		BuildId:         resp.FinishBuild.Id,
		Status:          resp.FinishBuild.Status,
		wallclockTimeMs: resp.FinishBuild.WallclockTimeMs,
	}, nil
}

type httpError struct {
	StatusCode int
	Body       string
}

func (e httpError) Error() string {
	return fmt.Sprintf("%s (http: %d)", e.Body, e.StatusCode)
}

func heartbeat(ctx context.Context, client *dockerclient.Client, req *http.Request) error {
	_, span := tracing.GetTracer().Start(ctx, "heartbeat")
	defer span.End()

	resp, err := client.HTTPClient().Do(req)
	if err != nil {
		tracing.RecordError(span, err, "failed to check heartbeat")
		return err
	}
	defer resp.Body.Close() // skipcq: GO-S2307

	span.SetAttributes(attribute.String("status_code", fmt.Sprintf("%d", resp.StatusCode)))
	if 200 <= resp.StatusCode && resp.StatusCode < 300 {
		return nil
	}

	if resp.StatusCode == http.StatusNotFound {
		tracing.RecordError(span, err, "no heartbeat endpoint")
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return &httpError{StatusCode: resp.StatusCode, Body: err.Error()}
	}

	return &httpError{StatusCode: resp.StatusCode, Body: string(b)}
}

// For remote builders send a periodic heartbeat during build to ensure machine stays alive
// This is a noop for local builders
func (r *Resolver) StartHeartbeat(ctx context.Context) (*StopSignal, error) {
	ctx, span := tracing.GetTracer().Start(ctx, "start_heartbeat")
	defer span.End()

	if !r.dockerFactory.remote {
		span.AddEvent("won't check heartbeart of non-remote build")
		return nil, nil
	}

	errMsg := "Failed to start remote builder heartbeat: %v\n"
	dockerClient, err := r.dockerFactory.buildFn(ctx, nil)
	if err != nil {
		terminal.Warnf(errMsg, err)
		return nil, nil
	}
	heartbeatUrl, err := getHeartbeatUrl(dockerClient)
	if err != nil {
		terminal.Warnf(errMsg, err)
		tracing.RecordError(span, err, "failed to get heartbeaturl")
		return nil, nil
	}

	span.SetAttributes(attribute.String("heartbeat_url", heartbeatUrl))
	heartbeatReq, err := http.NewRequestWithContext(ctx, http.MethodGet, heartbeatUrl, http.NoBody)
	if err != nil {
		terminal.Warnf(errMsg, err)
		tracing.RecordError(span, err, "failed to get http request")
		return nil, nil
	}
	heartbeatReq.SetBasicAuth(r.dockerFactory.appName, config.Tokens(ctx).Docker())
	heartbeatReq.Header.Set("User-Agent", fmt.Sprintf("flyctl/%s", buildinfo.Version().String()))

	terminal.Debugf("Sending remote builder heartbeat pulse to %s...\n", heartbeatUrl)

	span.AddEvent("sending first heartbeat")
	err = heartbeat(ctx, dockerClient, heartbeatReq)
	if err != nil {
		var h *httpError
		if errors.As(err, &h) {
			if h.StatusCode == http.StatusNotFound {
				terminal.Debugf("This builder doesn't have the heartbeat endpoint %s\n", heartbeatUrl)
				return nil, nil
			}
		} else {
			terminal.Debugf("not http error: err = %+v", err)
		}
		return nil, err
	}

	span.AddEvent("sending second heartbeat")
	resp, err := dockerClient.HTTPClient().Do(heartbeatReq)
	if err != nil {
		terminal.Debugf("Remote builder heartbeat pulse failed, not going to run heartbeat: %v\n", err)
		tracing.RecordError(span, err, "Remote builder heartbeat pulse failed, not going to run heartbeat")
		return nil, nil
	} else if resp.StatusCode != http.StatusAccepted {
		terminal.Debugf("Unexpected remote builder heartbeat response, not going to run heartbeat: %s\n", resp.Status)
		span.SetAttributes(attribute.String("status_code", fmt.Sprintf("%d", resp.StatusCode)))
		tracing.RecordError(span, err, "Remote builder heartbeat pulse failed, not going to run heartbeat")
		return nil, nil
	}

	pulseInterval := 30 * time.Second
	maxTime := 1 * time.Hour

	done := StopSignal{Chan: make(chan struct{})}
	time.AfterFunc(maxTime, func() { done.Stop() })

	go func() {
		defer dockerClient.Close() // skipcq: GO-S2307
		pulse := time.NewTicker(pulseInterval)
		defer pulse.Stop()
		defer done.Stop()

		for {
			select {
			case <-done.Chan:
				return
			case <-ctx.Done():
				return
			case <-pulse.C:
				terminal.Debugf("Sending remote builder heartbeat pulse to %s...\n", heartbeatUrl)
				err := heartbeat(ctx, dockerClient, heartbeatReq)
				if err != nil {
					terminal.Debugf("Remote builder heartbeat pulse failed: %v\n", err)
				}
			}
		}
	}()
	return &done, nil
}

func getHeartbeatUrl(dockerClient *dockerclient.Client) (string, error) {
	daemonHost := dockerClient.DaemonHost()
	parsed, err := url.Parse(daemonHost)
	if err != nil {
		return "", err
	}
	hostPort := parsed.Host
	host, _, _ := net.SplitHostPort(hostPort)
	parsed.Host = host + ":8080"
	parsed.Scheme = "http"
	parsed.Path = "/flyio/v1/extendDeadline"
	return parsed.String(), nil
}

func (s *StopSignal) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.Chan)
	})
}

func NewResolver(daemonType DockerDaemonType, apiClient *api.Client, appName string, iostreams *iostreams.IOStreams) *Resolver {
	return &Resolver{
		dockerFactory: newDockerClientFactory(daemonType, apiClient, appName, iostreams),
		apiClient:     apiClient,
	}
}

type imageBuilder interface {
	Name() string
	Run(ctx context.Context, dockerFactory *dockerClientFactory, streams *iostreams.IOStreams, opts ImageOptions, build *build) (*DeploymentImage, string, error)
}

type imageResolver interface {
	Name() string
	Run(ctx context.Context, dockerFactory *dockerClientFactory, streams *iostreams.IOStreams, opts RefOptions, build *build) (*DeploymentImage, string, error)
}
