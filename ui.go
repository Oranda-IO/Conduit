package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type appsResponse struct {
	Apps         []appRouteInfo `json:"apps"`
	Unmapped     []int          `json:"unmapped_running_ports"`
	SettingsFile string         `json:"settings_file"`
	UpdatedAt    string         `json:"updated_at"`
}

type appRouteInfo struct {
	Name      string `json:"name"`
	Port      int    `json:"port"`
	Running   bool   `json:"running"`
	NamedPath string `json:"named_path"`
	PortPath  string `json:"port_path"`
	NamedURL  string `json:"named_url,omitempty"`
	PortURL   string `json:"port_url,omitempty"`
}

type uiModel struct {
	Error        string
	Saved        string
	SettingsFile string
	Apps         []appRouteInfo
	Unmapped     []int
}

func (s *Server) handleApps(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleAppsGet(w, r)
	case http.MethodPost:
		s.handleAppsPost(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleAppsGet(w http.ResponseWriter, r *http.Request) {
	if err := s.settings.Load(); err != nil {
		log.Printf("settings load warning: %v", err)
	}

	payload := s.currentAppsResponse(r)
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleAppsPost(w http.ResponseWriter, r *http.Request) {
	if err := s.settings.Load(); err != nil {
		log.Printf("settings load warning: %v", err)
	}

	action := "set"
	name := ""
	port := 0
	redirect := ""

	contentType := r.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		var body struct {
			Action   string `json:"action"`
			Name     string `json:"name"`
			Port     int    `json:"port"`
			Redirect string `json:"redirect"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
			return
		}
		if body.Action != "" {
			action = body.Action
		}
		name = body.Name
		port = body.Port
		redirect = body.Redirect
	} else {
		if err := r.ParseForm(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form body"})
			return
		}
		if formAction := strings.TrimSpace(r.FormValue("action")); formAction != "" {
			action = formAction
		}
		name = r.FormValue("name")
		portText := strings.TrimSpace(r.FormValue("port"))
		if portText != "" {
			parsedPort, err := strconv.Atoi(portText)
			if err != nil {
				s.redirectWithStatus(w, r, redirect, "invalid port", http.StatusBadRequest)
				return
			}
			port = parsedPort
		}
		redirect = r.FormValue("redirect")
	}

	action = strings.ToLower(strings.TrimSpace(action))
	if action != "set" && action != "delete" {
		s.redirectWithStatus(w, r, redirect, "action must be set or delete", http.StatusBadRequest)
		return
	}

	var normalized string
	var err error
	if action == "set" {
		normalized, err = s.settings.Set(name, port)
	} else {
		normalized, err = s.settings.Delete(name)
	}
	if err != nil {
		s.redirectWithStatus(w, r, redirect, err.Error(), http.StatusBadRequest)
		return
	}

	if redirect != "" {
		http.Redirect(w, r, fmt.Sprintf("%s?saved=%s", redirect, url.QueryEscape(normalized)), http.StatusSeeOther)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"action": action,
		"name":   normalized,
		"port":   port,
	})
}

func (s *Server) redirectWithStatus(w http.ResponseWriter, r *http.Request, redirectPath, errMsg string, status int) {
	if redirectPath != "" {
		http.Redirect(w, r, fmt.Sprintf("%s?error=%s", redirectPath, url.QueryEscape(errMsg)), http.StatusSeeOther)
		return
	}
	writeJSON(w, status, map[string]string{"error": errMsg})
}

func (s *Server) currentAppsResponse(r *http.Request) appsResponse {
	snapshot := s.ports.snapshot()
	apps := s.settings.List(snapshot)
	baseURL := baseURLFromRequest(r)

	appRoutes := make([]appRouteInfo, 0, len(apps))
	mappedPorts := map[int]struct{}{}
	for _, app := range apps {
		mappedPorts[app.Port] = struct{}{}
		info := appRouteInfo{
			Name:      app.Name,
			Port:      app.Port,
			Running:   app.Running,
			NamedPath: "/" + app.Name + "/",
			PortPath:  fmt.Sprintf("/%d/", app.Port),
		}
		if baseURL != "" {
			info.NamedURL = baseURL + info.NamedPath
			info.PortURL = baseURL + info.PortPath
		}
		appRoutes = append(appRoutes, info)
	}

	unmapped := make([]int, 0)
	for _, port := range sortedPorts(snapshot) {
		if _, ok := mappedPorts[port]; !ok {
			unmapped = append(unmapped, port)
		}
	}

	return appsResponse{
		Apps:         appRoutes,
		Unmapped:     unmapped,
		SettingsFile: s.settings.Path(),
		UpdatedAt:    s.ports.LastUpdated().UTC().Format(time.RFC3339),
	}
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := s.settings.Load(); err != nil {
		log.Printf("settings load warning: %v", err)
	}

	payload := s.currentAppsResponse(r)
	model := uiModel{
		Error:        r.URL.Query().Get("error"),
		Saved:        r.URL.Query().Get("saved"),
		SettingsFile: payload.SettingsFile,
		Apps:         payload.Apps,
		Unmapped:     payload.Unmapped,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplate.Execute(w, model); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func baseURLFromRequest(r *http.Request) string {
	host := strings.TrimSpace(r.Host)
	if host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + host
}

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Conduit UI</title>
  <style>
    :root {
      --bg: #f7fafc;
      --panel: #ffffff;
      --ink: #102135;
      --muted: #5f7186;
      --line: #d7e1ea;
      --accent: #0b66d6;
      --accent-soft: #e6f0fe;
      --ok: #117a42;
      --warn: #b85000;
      --mono: "JetBrains Mono", "SFMono-Regular", Menlo, Consolas, monospace;
      --sans: "Manrope", "Segoe UI", sans-serif;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: var(--sans);
      color: var(--ink);
      background:
        radial-gradient(circle at 0% 0%, #dff0ff 0, rgba(223,240,255,0) 40%),
        radial-gradient(circle at 100% 100%, #f4ffe7 0, rgba(244,255,231,0) 45%),
        var(--bg);
    }
    .wrap { max-width: 980px; margin: 0 auto; padding: 20px; }
    .hero {
      background: linear-gradient(135deg, #ffffff 0%, #f0f7ff 100%);
      border: 1px solid var(--line);
      border-radius: 14px;
      padding: 18px;
      box-shadow: 0 10px 30px rgba(21, 60, 94, 0.08);
    }
    h1 { margin: 0 0 6px; font-size: 24px; letter-spacing: -0.02em; }
    p { margin: 0; color: var(--muted); }
    .code { font-family: var(--mono); background: var(--accent-soft); padding: 2px 6px; border-radius: 6px; }
    .grid { display: grid; gap: 14px; margin-top: 14px; grid-template-columns: 1fr; }
    .card { background: var(--panel); border: 1px solid var(--line); border-radius: 12px; padding: 14px; }
    .card h2 { margin: 0 0 10px; font-size: 18px; }
    table { width: 100%; border-collapse: collapse; font-size: 14px; }
    th, td { padding: 10px 8px; border-bottom: 1px solid var(--line); text-align: left; vertical-align: top; }
    th { color: var(--muted); font-weight: 700; }
    .mono { font-family: var(--mono); }
    .status-up { color: var(--ok); font-weight: 700; }
    .status-down { color: var(--warn); font-weight: 700; }
    form.inline { display: inline-flex; gap: 8px; align-items: center; flex-wrap: wrap; }
    input, select, button {
      border: 1px solid #bccad9;
      border-radius: 8px;
      padding: 8px 10px;
      font: inherit;
      background: #fff;
    }
    button {
      background: var(--accent);
      color: #fff;
      border-color: var(--accent);
      cursor: pointer;
    }
    button.ghost {
      background: #fff;
      color: var(--ink);
      border-color: #c7d6e6;
    }
    .pill { display: inline-block; padding: 4px 8px; border-radius: 999px; font-size: 12px; background: #edf4fb; color: #304b67; }
    .flash { margin-top: 10px; padding: 9px 10px; border-radius: 8px; font-size: 14px; }
    .flash.err { background: #fff1e8; color: #8a2e00; border: 1px solid #ffd7c2; }
    .flash.ok { background: #ecfff3; color: #0f6137; border: 1px solid #c9f2d9; }
    .foot { margin-top: 14px; color: var(--muted); font-size: 13px; }
    a { color: var(--accent); text-decoration: none; }
    a:hover { text-decoration: underline; }
    @media (max-width: 760px) {
      .wrap { padding: 14px; }
      th:nth-child(4), td:nth-child(4) { display: none; }
    }
  </style>
</head>
<body>
  <div class="wrap">
    <section class="hero">
      <h1>Conduit Dashboard</h1>
      <p>Name running ports and connect fast with readable routes like <span class="code">/myapp/</span>.</p>
      <div class="foot">Settings file: <span class="mono">{{ .SettingsFile }}</span></div>
      {{ if .Error }}<div class="flash err">{{ .Error }}</div>{{ end }}
      {{ if .Saved }}<div class="flash ok">Saved: <span class="mono">{{ .Saved }}</span></div>{{ end }}
    </section>

    <div class="grid">
      <section class="card">
        <h2>Add or Update App Name</h2>
        <form method="post" action="/apps" class="inline">
          <input type="hidden" name="action" value="set">
          <input type="hidden" name="redirect" value="/ui">
          <input name="name" placeholder="app-name" required>
          <input name="port" type="number" min="1" max="65535" placeholder="3000" required>
          <button type="submit">Save Mapping</button>
        </form>
        <div class="foot">App names: lowercase letters, numbers, ., _, - (max 63 chars).</div>
      </section>

      <section class="card">
        <h2>Mapped Apps</h2>
        <table>
          <thead>
            <tr>
              <th>App</th>
              <th>Port</th>
              <th>Status</th>
              <th>Named Route</th>
              <th>Connect</th>
            </tr>
          </thead>
          <tbody>
            {{ if not .Apps }}
              <tr><td colspan="5">No app mappings yet.</td></tr>
            {{ else }}
              {{ range .Apps }}
                <tr>
                  <td><span class="mono">{{ .Name }}</span></td>
                  <td><span class="mono">{{ .Port }}</span></td>
                  <td>{{ if .Running }}<span class="status-up">running</span>{{ else }}<span class="status-down">offline</span>{{ end }}</td>
                  <td><span class="mono">{{ .NamedPath }}</span></td>
                  <td>
                    <a href="{{ .NamedPath }}">named</a>
                    <span class="pill">or</span>
                    <a href="{{ .PortPath }}">port</a>
                    <form method="post" action="/apps" class="inline" style="margin-top:8px;">
                      <input type="hidden" name="action" value="delete">
                      <input type="hidden" name="redirect" value="/ui">
                      <input type="hidden" name="name" value="{{ .Name }}">
                      <button type="submit" class="ghost">Remove</button>
                    </form>
                  </td>
                </tr>
              {{ end }}
            {{ end }}
          </tbody>
        </table>
      </section>

      <section class="card">
        <h2>Running Ports Without Names</h2>
        {{ if not .Unmapped }}
          <p>All running ports are mapped.</p>
        {{ else }}
          <p>
            {{ range .Unmapped }}
              <a class="mono" href="/{{ . }}/">/{{ . }}/</a>&nbsp;
            {{ end }}
          </p>
        {{ end }}
      </section>
    </div>
  </div>
</body>
</html>`))
