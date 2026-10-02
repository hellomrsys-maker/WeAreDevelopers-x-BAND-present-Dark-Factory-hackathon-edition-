package api

import (
	"net/http"
	"strings"

	"tablekeeper/contracts"
	"tablekeeper/store"
	"tablekeeper/web"
)

type Router struct {
	store        *store.Store
	authH        *AuthHandler
	testH        *TestHandler
	restH        *RestaurantsHandler
	resH         *ReservationsHandler
	mux          *http.ServeMux
	handlerStack http.Handler
}

func NewRouter(s *store.Store, c contracts.CalendarEngine, b contracts.BookingEngine) *Router {
	r := &Router{
		store: s,
		authH: NewAuthHandler(s),
		testH: NewTestHandler(s, c),
		restH: NewRestaurantsHandler(s, b, c),
		resH:  NewReservationsHandler(b, c),
		mux:   http.NewServeMux(),
	}
	r.setupRoutes()
	return r
}

func (r *Router) setupRoutes() {
	serveHTML := func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		w.Write(web.IndexHTML)
	}

	// Web UI Screen routes
	r.mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" && req.URL.Path != "/login" && req.URL.Path != "/signup" && req.URL.Path != "/lookup" && req.URL.Path != "/admin" && req.URL.Path != "/factory" {
			WriteError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		serveHTML(w, req)
	})

	r.mux.HandleFunc("/signup", serveHTML)
	r.mux.HandleFunc("/login", serveHTML)
	r.mux.HandleFunc("/admin", serveHTML)
	r.mux.HandleFunc("/factory", serveHTML)
	r.mux.HandleFunc("/lookup", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if req.URL.Query().Get("reference") != "" && strings.Contains(req.Header.Get("Accept"), "application/json") {
			r.resH.Lookup(w, req)
			return
		}
		serveHTML(w, req)
	})

	// Test & Health endpoints
	r.mux.HandleFunc("/health", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.testH.Health(w, req)
	})

	r.mux.HandleFunc("/_test/reset", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.testH.Reset(w, req)
	})

	r.mux.HandleFunc("/_test/export", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.testH.Export(w, req)
	})

	r.mux.HandleFunc("/_test/import", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.testH.Import(w, req)
	})

	// Auth endpoints
	r.mux.HandleFunc("/auth/signup", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.authH.Signup(w, req)
	})

	r.mux.HandleFunc("/auth/login", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.authH.Login(w, req)
	})

	// Public restaurant and availability endpoints
	r.mux.HandleFunc("/restaurants", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.restH.List(w, req)
	})

	publishPolicyHandler := AuthMiddleware(r.store, IdempotencyMiddleware(r.store, r.restH.PublishPolicy))
	createReplanHandler := AuthMiddleware(r.store, IdempotencyMiddleware(r.store, r.restH.CreateReplan))
	applyReplanHandler := AuthMiddleware(r.store, IdempotencyMiddleware(r.store, r.restH.ApplyReplan))

	r.mux.HandleFunc("/restaurants/", func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		if strings.HasSuffix(path, "/apply") && strings.Contains(path, "/replans/") {
			if req.Method != http.MethodPost {
				WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
				return
			}
			applyReplanHandler(w, req)
			return
		}

		if strings.HasSuffix(path, "/replans") {
			if req.Method != http.MethodPost {
				WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
				return
			}
			createReplanHandler(w, req)
			return
		}

		if strings.HasSuffix(path, "/policies") {
			switch req.Method {
			case http.MethodPost:
				publishPolicyHandler(w, req)
			case http.MethodGet:
				r.restH.ListPolicies(w, req)
			default:
				WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			}
			return
		}

		if req.Method != http.MethodGet {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.restH.Get(w, req)
	})

	r.mux.HandleFunc("/availability", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.restH.Availability(w, req)
	})

	// Protected reservation write operations with idempotency
	createResHandler := AuthMiddleware(r.store, IdempotencyMiddleware(r.store, r.resH.Create))
	moveResHandler := AuthMiddleware(r.store, IdempotencyMiddleware(r.store, r.resH.Moves))

	r.mux.HandleFunc("/reservation-moves", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		moveResHandler(w, req)
	})

	// /reservations dispatcher
	r.mux.HandleFunc("/reservations", func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodPost:
			createResHandler(w, req)
		case http.MethodGet:
			AuthMiddleware(r.store, r.resH.List)(w, req)
		default:
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
	})

	r.mux.HandleFunc("/reservations/", func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		if strings.HasSuffix(path, "/cancel") {
			if req.Method != http.MethodPost {
				WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
				return
			}
			AuthMiddleware(r.store, r.resH.Cancel)(w, req)
			return
		}

		if strings.HasSuffix(path, "/decision") {
			if req.Method != http.MethodGet {
				WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
				return
			}
			OptionalAuthMiddleware(r.store, r.resH.Decision)(w, req)
			return
		}

		if strings.HasSuffix(path, "/history") {
			if req.Method != http.MethodGet {
				WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
				return
			}
			OptionalAuthMiddleware(r.store, r.resH.History)(w, req)
			return
		}

		switch req.Method {
		case http.MethodGet:
			AuthMiddleware(r.store, r.resH.Get)(w, req)
		case http.MethodPatch:
			AuthMiddleware(r.store, r.resH.Patch)(w, req)
		default:
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
	})

	// Series endpoints
	createSeriesHandler := AuthMiddleware(r.store, IdempotencyMiddleware(r.store, r.resH.CreateSeries))
	amendSeriesHandler := AuthMiddleware(r.store, IdempotencyMiddleware(r.store, r.resH.AmendSeries))

	r.mux.HandleFunc("/series", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		createSeriesHandler(w, req)
	})

	r.mux.HandleFunc("/series/", func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/amend") {
			if req.Method != http.MethodPost {
				WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
				return
			}
			amendSeriesHandler(w, req)
			return
		}

		if req.Method != http.MethodGet {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		OptionalAuthMiddleware(r.store, r.resH.GetSeries)(w, req)
	})
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}
