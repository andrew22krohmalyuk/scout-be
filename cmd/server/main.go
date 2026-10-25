package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/andrew22krohmalyuk/scout-be/internal/db"
	// "github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

const (
	sessionCookie = "scout_session"
	appBaseURL    = "https://app.scout.localhost"
)

type sessionUser struct {
	ID      string `json:"id"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Picture string `json:"picture,omitempty"`
}

// Google OIDC relying-party credentials, filled from the environment in main.
// Not a const block: a client secret in tracked source is a secret you have to
// rotate. See .env.example.
var googleOIDC struct {
	clientID     string
	clientSecret string
}

// Stands in for the `users` row Phase 1 will upsert from Google's `sub`.
var mockUser = sessionUser{
	ID:    "01920000-0000-7000-8000-000000000001",
	Email: "dev@scout.app",
	Name:  "Dev User",
}

func main() {
	log.Println("Main start ...")

	db.Connect()
	// godotenv resolves .env relative to the working directory, so the server
	// has to start from the repo root — which is what `make run` does. A
	// missing file is not fatal: deployed environments inject the same keys
	// directly and never ship a .env.
	if err := godotenv.Load(); err != nil {
		log.Printf("no .env loaded (%v); falling back to the ambient environment", err)
	}
	googleOIDC.clientID = os.Getenv("GOOGLE_CLIENT_ID")
	googleOIDC.clientSecret = os.Getenv("GOOGLE_CLIENT_SECRET")

	gin.SetMode(gin.DebugMode)
	r := gin.Default()

	// Unversioned: liveness is infrastructure, not API.
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	v1 := r.Group("/v1")

	// // Real flow: mint state + nonce + PKCE verifier into a short-lived cookie,
	// // then 302 to Google. The mock skips Google and issues the session directly.
	v1.GET("/auth/login", func(c *gin.Context) {
		// The real flow builds Google's authorization URL from these. The mock
		// below does not need them, but refuse here anyway: an unconfigured
		// server should fail on the route that depends on the config, loudly,
		// rather than hand out sessions Google never saw.
		if googleOIDC.clientID == "" || googleOIDC.clientSecret == "" {
			log.Println("auth/login: GOOGLE_CLIENT_ID or GOOGLE_CLIENT_SECRET is unset; see .env.example")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "google oidc is not configured"})
			return
		}

		// provider, err := oidc.NewProvider(ctx, "https://accounts.google.com")

		returnTo := safeReturnPath(c.Query("return_to"), "/home")
		setSessionCookie(c, "mock-session-token")
		c.Redirect(http.StatusFound, appBaseURL+returnTo)
	})

	// // Real flow: verify state, exchange the code, verify nonce, upsert the
	// // identity, then issue the session. Nothing calls this until Google is wired.
	v1.GET("/auth/callback", func(c *gin.Context) {
		setSessionCookie(c, "mock-session-token")
		c.Redirect(http.StatusFound, appBaseURL+"/home")
	})

	// // scout-fe reads any non-2xx as "signed out", so 401 is the answer here
	// // rather than an error body it would have to parse.
	v1.GET("/auth/me", func(c *gin.Context) {
		ck, err := c.Request.Cookie(sessionCookie)
		if err != nil || ck.Value == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "no active session"})
			return
		}
		c.JSON(http.StatusOK, mockUser)
	})

	// // Real flow: revoke the sessions row first — clearing the cookie alone
	// // leaves an already-stolen session id valid until it expires.
	v1.POST("/auth/logout", func(c *gin.Context) {
		clearSessionCookie(c)
		c.Status(http.StatusNoContent)
	})

	log.Println("listening on :8080")
	if err := r.Run(":8080"); err != nil {
		log.Fatal(err)
	}
}

// http.SetCookie rather than c.SetCookie: gin's helper reads SameSite from
// engine-level state instead of taking it per call.
func setSessionCookie(c *gin.Context, value string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   int((7 * 24 * time.Hour).Seconds()),
		HttpOnly: true,
		// Must stay false on http://localhost or the browser drops the cookie
		// silently. Drive it from config once prod exists.
		Secure: false,
		// Lax, never Strict: Google's callback is a cross-site top-level
		// navigation, and Strict would drop the cookie on exactly that hop.
		SameSite: http.SameSiteLaxMode,
	})
}

// safeReturnPath mirrors scout-fe's safeRedirectPath. Without it the redirect
// target is attacker-controlled and the endpoint is an open redirect on an
// auth path. Browsers normalise "/\evil.com" into a protocol-relative URL, so
// the "//" check alone is bypassable.
func safeReturnPath(to, fallback string) string {
	if to == "" ||
		!strings.HasPrefix(to, "/") ||
		strings.HasPrefix(to, "//") ||
		strings.HasPrefix(to, `/\`) {
		return fallback
	}
	return to
}

func clearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   false,
		SameSite: http.SameSiteLaxMode,
	})
}
