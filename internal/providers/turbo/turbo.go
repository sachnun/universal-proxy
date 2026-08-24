// Package turbo implements the Turbo VPN provider (free tier).
// Free servers are HTTPS CONNECT proxies on :443 with static shared
// credentials baked into the extension; the server list is fetched from the
// Turbo API with a verified static fallback.
package turbo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"unroxy/internal/core"
	"unroxy/internal/providers"
)

const (
	serverListURL = "https://turbovpn.com/api/mms/serverlist/v1/webext/servers_list"
	proxyUser     = "testuser1"
	proxyPass     = "e4b72b531a2d10900519"
)

// Provider maintains the Turbo VPN free proxy fleet.
type Provider struct {
	host   *providers.Host
	logger *log.Logger
}

func init() {
	providers.Register(&Provider{})
}

func (p *Provider) Name() string { return "Turbo" }

func (p *Provider) Start(ctx context.Context, host *providers.Host, logger *log.Logger) error {
	p.host = host
	p.logger = logger

	servers, err := fetchServers()
	if err != nil {
		logger.Printf("Turbo: server list fetch failed (%v), using static fallback", err)
		servers = fallbackServers()
	}
	logger.Printf("Turbo: %d servers", len(servers))

	states := buildStates(servers)
	for _, ps := range states {
		ps.Priority = 2
	}
	// The background validator probes the fleet; healthy servers are
	// graduated into the pools.
	host.Submit(states)
	logger.Printf("Turbo: %d proxies queued for validation", len(states))
	return nil
}

type turboServer struct {
	host    string
	port    int
	country string
}

// fetchServers pulls the free server list from the Turbo API.
func fetchServers() ([]turboServer, error) {
	body, _ := json.Marshal(map[string]string{
		"country": "", "user_ip": "", "os_lang": "", "login_id": "",
	})
	req, err := http.NewRequest(http.MethodPost, serverListURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-App-Type", "302")
	req.Header.Set("X-App-Ver-Code", "202607111356")

	resp, err := core.NewProviderHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("turbovpn server list status %d", resp.StatusCode)
	}

	var payload struct {
		Servers []struct {
			Country string `json:"country"`
			Service struct {
				Ports    []int    `json:"ports"`
				Hosts    []string `json:"hostnames"`
				Hostname string   `json:"hostname"`
			} `json:"service_config"`
		} `json:"servers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("turbovpn decode: %w", err)
	}

	servers := make([]turboServer, 0, len(payload.Servers))
	for _, s := range payload.Servers {
		host := s.Service.Hostname
		if len(s.Service.Hosts) > 0 {
			host = s.Service.Hosts[0]
		}
		if host == "" {
			continue
		}
		port := 443
		if len(s.Service.Ports) > 0 {
			port = s.Service.Ports[0]
		}
		servers = append(servers, turboServer{host: host, port: port, country: strings.ToUpper(s.Country)})
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("no servers in turbovpn response")
	}
	return servers, nil
}

// fallbackServers are verified free servers from the extension bundle.
func fallbackServers() []turboServer {
	return []turboServer{
		{host: "8001ba7b.acsnet.co", port: 443, country: "SG"},
		{host: "4f6e36d3.acsnet.co", port: 443, country: "US"},
		{host: "a213cd1c.achoon.com", port: 443, country: "DE"},
	}
}

func buildStates(servers []turboServer) []*core.ProxyState {
	states := make([]*core.ProxyState, 0, len(servers))
	for _, s := range servers {
		u := &url.URL{
			Scheme: "https",
			Host:   net.JoinHostPort(s.host, strconv.Itoa(s.port)),
			User:   url.UserPassword(proxyUser, proxyPass),
		}
		state := &core.ProxyState{
			Key:     u.String(),
			URL:     u,
			Country: s.country,
		}
		state.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return core.HTTPProxyConnect(ctx, u, addr)
		}
		states = append(states, state)
	}
	return states
}
