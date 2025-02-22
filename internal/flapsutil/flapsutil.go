package flapsutil

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	fly "github.com/superfly/fly-go"
	"github.com/superfly/fly-go/flaps"
	"github.com/superfly/flyctl/agent"
	"github.com/superfly/flyctl/internal/buildinfo"
	"github.com/superfly/flyctl/internal/config"
	"github.com/superfly/flyctl/internal/logger"
)

func NewClientWithOptions(ctx context.Context, opts flaps.NewClientOpts) (*flaps.Client, error) {
	// Connect over wireguard depending on FLAPS URL.
	if strings.TrimSpace(strings.ToLower(os.Getenv("FLY_FLAPS_BASE_URL"))) == "peer" {
		orgSlug, err := resolveOrgSlugForApp(ctx, opts.AppCompact, opts.AppName)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve org for app '%s': %w", opts.AppName, err)
		}

		client := fly.ClientFromContext(ctx)
		agentclient, err := agent.Establish(ctx, client)
		if err != nil {
			return nil, fmt.Errorf("error establishing agent: %w", err)
		}

		dialer, err := agentclient.Dialer(ctx, orgSlug)
		if err != nil {
			return nil, fmt.Errorf("flaps: can't build tunnel for %s: %w", orgSlug, err)
		}
		opts.DialContext = dialer.DialContext

		flapsBaseUrlString := fmt.Sprintf("http://[%s]:4280", resolvePeerIP(dialer.State().Peer.Peerip))
		if opts.BaseURL, err = url.Parse(flapsBaseUrlString); err != nil {
			return nil, fmt.Errorf("failed to parse flaps url '%s' with error: %w", flapsBaseUrlString, err)
		}
	}

	if opts.UserAgent == "" {
		opts.UserAgent = buildinfo.UserAgent()
	}

	if opts.Tokens == nil {
		opts.Tokens = config.Tokens(ctx)
	}

	if v := logger.MaybeFromContext(ctx); v != nil {
		opts.Logger = v
	}

	return flaps.NewWithOptions(ctx, opts)
}

func resolveOrgSlugForApp(ctx context.Context, app *fly.AppCompact, appName string) (string, error) {
	app, err := resolveApp(ctx, app, appName)
	if err != nil {
		return "", err
	}
	return app.Organization.Slug, nil
}

func resolveApp(ctx context.Context, app *fly.AppCompact, appName string) (*fly.AppCompact, error) {
	var err error
	if app == nil {
		client := fly.ClientFromContext(ctx)
		app, err = client.GetAppCompact(ctx, appName)
	}
	return app, err
}

func resolvePeerIP(ip string) string {
	peerIP := net.ParseIP(ip)
	var natsIPBytes [16]byte
	copy(natsIPBytes[0:], peerIP[0:6])
	natsIPBytes[15] = 3
	return net.IP(natsIPBytes[:]).String()
}
