package caddy

import (
	"encoding/json"
	"sort"
)

type AppRoute struct {
	Slug     string
	Hostname string
	Upstream string
}

const AdminSocket = "unix//var/run/caddy/admin.sock"

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

func Render(routes []AppRoute) ([]byte, error) {
	// Sort routes by Slug
	sorted := make([]AppRoute, len(routes))
	copy(sorted, routes)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Slug < sorted[j].Slug
	})

	// Build config routes
	caddy_routes := make([]Route, len(sorted))
	for i, route := range sorted {
		caddy_routes[i] = Route{
			Match: []HostMatch{
				{
					Host: []string{route.Hostname},
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
		}
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
