package api

import (
	"net/http"
	"strings"

	"tablekeeper/contracts"
	"tablekeeper/store"
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
		restH: NewRestaurantsHandler(s, b),
		resH:  NewReservationsHandler(b, c),
		mux:   http.NewServeMux(),
	}
	r.setupRoutes()
	return r
}

func (r *Router) setupRoutes() {
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

	r.mux.HandleFunc("/restaurants/", func(w http.ResponseWriter, req *http.Request) {
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

	r.mux.HandleFunc("/lookup", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.resH.Lookup(w, req)
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

		switch req.Method {
		case http.MethodGet:
			AuthMiddleware(r.store, r.resH.Get)(w, req)
		case http.MethodPatch:
			AuthMiddleware(r.store, r.resH.Patch)(w, req)
		default:
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
	})
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mux.ServeHTTP(w, req)
}
