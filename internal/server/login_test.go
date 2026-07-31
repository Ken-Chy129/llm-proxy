package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ken-Chy129/llm-proxy/internal/config"
	"github.com/gin-gonic/gin"
)

func newLoginRouter(user, password string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Server.AdminUser = user
	cfg.Server.AdminPassword = password
	r := gin.New()
	r.GET("/login", loginPage())
	r.POST("/login", loginHandler(cfg))
	return r
}

func postLogin(t *testing.T, r *gin.Engine, user, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := "username=" + user + "&password=" + password
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func sessionCookie(w *httptest.ResponseRecorder) string {
	for _, c := range w.Result().Cookies() {
		if c.Name == "session" && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

func TestLoginAcceptsConfiguredCredentials(t *testing.T) {
	r := newLoginRouter("admin", "s3cret")

	w := postLogin(t, r, "admin", "s3cret")
	if got := w.Header().Get("Location"); got != "/" {
		t.Errorf("redirect = %q, want %q", got, "/")
	}
	if sessionCookie(w) == "" {
		t.Error("no session cookie issued on a correct login")
	}

	for name, creds := range map[string][2]string{
		"wrong password": {"admin", "nope"},
		"wrong user":     {"root", "s3cret"},
		"blank form":     {"", ""},
	} {
		w := postLogin(t, r, creds[0], creds[1])
		if c := sessionCookie(w); c != "" {
			t.Errorf("%s issued a session cookie (%q)", name, c)
		}
	}
}

// A fresh install has admin_user/admin_password commented out. Comparing a blank
// form against blank config used to succeed, handing the dashboard to anyone who
// found the port; unconfigured must mean closed.
func TestLoginRefusedWhenCredentialsUnconfigured(t *testing.T) {
	for name, creds := range map[string][2]string{
		"both empty":     {"", ""},
		"password empty": {"admin", ""},
		"user empty":     {"", "s3cret"},
	} {
		r := newLoginRouter(creds[0], creds[1])
		for _, attempt := range [][2]string{{"", ""}, {"admin", "s3cret"}, {"admin", ""}} {
			w := postLogin(t, r, attempt[0], attempt[1])
			if c := sessionCookie(w); c != "" {
				t.Errorf("%s: login as %q/%q succeeded (cookie %q), want refusal",
					name, attempt[0], attempt[1], c)
			}
			if loc := w.Header().Get("Location"); !strings.Contains(loc, "not+configured") {
				t.Errorf("%s: redirect = %q, want the not-configured hint", name, loc)
			}
		}
	}
}

// The login page splices ?error= into a raw HTML string, so it must escape it —
// otherwise a phishing link renders arbitrary markup on an unauthenticated page.
func TestLoginPageEscapesErrorQuery(t *testing.T) {
	r := newLoginRouter("admin", "s3cret")
	req := httptest.NewRequest(http.MethodGet, `/login?error=<script>alert(1)</script>`, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Error("error query reflected unescaped — reflected XSS")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("escaped error text not rendered at all; body contains: %q",
			body[max(0, strings.Index(body, `class="err"`)):min(len(body), strings.Index(body, `class="err"`)+80)])
	}
}
