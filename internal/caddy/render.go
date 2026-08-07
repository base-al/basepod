package caddy

import (
	"encoding/json"
	"sort"
)

type AppRoute struct {
	Slug      string
	Hostnames []string
	Upstream  string
}

// DashboardRoute is the (optional) route Render prepends for BasePod's own
// web dashboard, proxying Hostname to Upstream (the control plane's
// gateway-facing listener — see internal/server.Run). A nil DashboardRoute
// passed to Render omits the route entirely.
type DashboardRoute struct {
	Hostname string
	Upstream string
}

// AdminSocketPath is the absolute path, inside the bp-caddy container, of
// Caddy's admin API unix socket — the container-local counterpart to
// AdminSocket's Caddy-dial-string form below ("unix/" + this path,
// mirroring DashboardSockDial's own "unix/" + path pattern). Manager.Health
// probes this path directly via `curl --unix-socket` (through the same
// podman-exec path Apply uses for reloads): the dial-string form is what
// Caddy's own JSON config wants, but a plain filesystem path is what a
// curl argument wants, so both forms are kept as named constants rather
// than one being derived ad hoc from the other at each call site.
const AdminSocketPath = "/var/run/caddy/admin.sock"

const AdminSocket = "unix/" + AdminSocketPath

// Typed structs for stable JSON field ordering
type Admin struct {
	Listen string `json:"listen"`
}

type Upstream struct {
	Dial string `json:"dial"`
}

type ReverseProxyHandler struct {
	Handler   string     `json:"handler"`
	Upstreams []Upstream `json:"upstreams"`
}

type HostMatch struct {
	Host []string `json:"host"`
}

type Route struct {
	Match    []HostMatch           `json:"match"`
	Handle   []ReverseProxyHandler `json:"handle"`
	Terminal bool                  `json:"terminal"`
}

type Server struct {
	Listen []string `json:"listen"`
	Routes []Route  `json:"routes"`
}

type Servers struct {
	Main Server `json:"main"`
}

type Http struct {
	Servers Servers `json:"servers"`
}

type Apps struct {
	Http Http `json:"http"`
}

type Config struct {
	Admin Admin `json:"admin"`
	Apps  Apps  `json:"apps"`
}

func Render(routes []AppRoute, dashboard *DashboardRoute) ([]byte, error) {
	// Sort routes by Slug
	sorted := make([]AppRoute, len(routes))
	copy(sorted, routes)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Slug < sorted[j].Slug
	})

	// Build config routes. The dashboard route, if any, is placed first so
	// it takes priority over any app route that might otherwise also match
	// its hostname (it never should, given the collision checks in
	// internal/api, but first-and-terminal keeps that invariant enforced
	// here too rather than relying solely on those checks).
	caddy_routes := make([]Route, 0, len(sorted)+1)
	if dashboard != nil {
		caddy_routes = append(caddy_routes, Route{
			Match: []HostMatch{
				{
					Host: []string{dashboard.Hostname},
				},
			},
			Handle: []ReverseProxyHandler{
				{
					Handler: "reverse_proxy",
					Upstreams: []Upstream{
						{
							Dial: dashboard.Upstream,
						},
					},
				},
			},
			Terminal: true,
		})
	}
	for _, route := range sorted {
		caddy_routes = append(caddy_routes, Route{
			Match: []HostMatch{
				{
					Host: route.Hostnames,
				},
			},
			Handle: []ReverseProxyHandler{
				{
					Handler: "reverse_proxy",
					Upstreams: []Upstream{
						{
							Dial: route.Upstream,
						},
					},
				},
			},
			Terminal: true,
		})
	}

	cfg := Config{
		Admin: Admin{
			Listen: AdminSocket,
		},
		Apps: Apps{
			Http: Http{
				Servers: Servers{
					Main: Server{
						Listen: []string{":80", ":443"},
						Routes: caddy_routes,
					},
				},
			},
		},
	}

	return json.MarshalIndent(cfg, "", "  ")
}
