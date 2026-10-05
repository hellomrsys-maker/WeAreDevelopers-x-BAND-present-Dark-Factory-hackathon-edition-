package api

import (
	"net/http"
	"strings"

	"tablekeeper/contracts"
	"tablekeeper/engines/money"
	"tablekeeper/engines/payments"
	"tablekeeper/store"
	"tablekeeper/web"
)

type Router struct {
	store        *store.Store
	authH        *AuthHandler
	testH        *TestHandler
	restH        *RestaurantsHandler
	resH         *ReservationsHandler
	agentH       *AgentHandler
	payH         *PaymentHandler
	engine       *ContinuousEngine
	mux          *http.ServeMux
	handlerStack http.Handler
}

func NewRouter(s *store.Store, c contracts.CalendarEngine, b contracts.BookingEngine) *Router {
	mEngine := money.NewEngine(s)
	pEngine := payments.NewEngine(s, mEngine)
	payH := NewPaymentHandler(s, mEngine, pEngine)
	cEngine := InitContinuousEngine(payH)
	r := &Router{
		store:  s,
		authH:  NewAuthHandler(s),
		testH:  NewTestHandler(s, c),
		restH:  NewRestaurantsHandler(s, b, c),
		resH:   NewReservationsHandler(b, c),
		agentH: NewAgentHandler(s, b, c),
		payH:   payH,
		engine: cEngine,
		mux:    http.NewServeMux(),
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
		p := req.URL.Path
		if p != "/" && p != "/login" && p != "/signup" && p != "/lookup" && p != "/admin" && p != "/factory" && p != "/agents" && p != "/telemetry" && p != "/rewards" && p != "/host" && p != "/shifts" && p != "/agent-api" && p != "/client" && p != "/clients" && p != "/users" && p != "/payments" && p != "/payment" && p != "/trajectory" {
			WriteError(w, http.StatusNotFound, "not_found", "not found")
			return
		}
		serveHTML(w, req)
	})

	r.mux.HandleFunc("/signup", serveHTML)
	r.mux.HandleFunc("/login", serveHTML)
	r.mux.HandleFunc("/admin", serveHTML)
	r.mux.HandleFunc("/factory", serveHTML)
	r.mux.HandleFunc("/agents", serveHTML)
	r.mux.HandleFunc("/telemetry", serveHTML)
	r.mux.HandleFunc("/rewards", serveHTML)
	r.mux.HandleFunc("/host", serveHTML)
	r.mux.HandleFunc("/shifts", serveHTML)
	r.mux.HandleFunc("/agent-api", serveHTML)
	r.mux.HandleFunc("/client", serveHTML)
	r.mux.HandleFunc("/clients", serveHTML)
	r.mux.HandleFunc("/users", serveHTML)
	r.mux.HandleFunc("/payments", serveHTML)
	r.mux.HandleFunc("/payment", serveHTML)
	r.mux.HandleFunc("/trajectory", serveHTML)

	// 100 Virtual Users Fleet & Order Placement API
	r.mux.HandleFunc("/api/users/fleet", func(w http.ResponseWriter, req *http.Request) {
		r.payH.ListFleetUsers(w, req)
	})
	r.mux.HandleFunc("/api/users/detail", func(w http.ResponseWriter, req *http.Request) {
		r.payH.GetUserDetail(w, req)
	})
	r.mux.HandleFunc("/api/payment/order", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.payH.PlaceOrder(w, req)
	})
	r.mux.HandleFunc("/api/payment/orders", func(w http.ResponseWriter, req *http.Request) {
		r.payH.ListUserOrders(w, req)
	})

	// Continuous Agent Communication, Rankings & Charts API
	r.mux.HandleFunc("/api/agent/continuous-start", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.engine.HandleContinuousStart(w, req)
	})
	r.mux.HandleFunc("/api/agent/continuous-stop", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.engine.HandleContinuousStop(w, req)
	})
	r.mux.HandleFunc("/api/agent/continuous-status", func(w http.ResponseWriter, req *http.Request) {
		r.engine.HandleContinuousStatus(w, req)
	})
	r.mux.HandleFunc("/api/agent/messages", func(w http.ResponseWriter, req *http.Request) {
		r.engine.HandleAgentMessages(w, req)
	})
	r.mux.HandleFunc("/api/rankings/live", func(w http.ResponseWriter, req *http.Request) {
		r.engine.HandleLiveRankings(w, req)
	})
	r.mux.HandleFunc("/api/charts/metrics", func(w http.ResponseWriter, req *http.Request) {
		r.engine.HandleChartMetrics(w, req)
	})
	r.mux.HandleFunc("/api/agent/band-sync", func(w http.ResponseWriter, req *http.Request) {
		r.engine.HandleBandSyncStatus(w, req)
	})
	r.mux.HandleFunc("/api/agent/band-sync/trigger", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.engine.HandleBandSyncTrigger(w, req)
	})

	// Payment & Wallet API endpoints
	r.mux.HandleFunc("/api/payment/wallet", func(w http.ResponseWriter, req *http.Request) {
		r.payH.GetWallet(w, req)
	})
	r.mux.HandleFunc("/api/payment/charge", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.payH.Charge(w, req)
	})
	r.mux.HandleFunc("/api/payment/topup", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.payH.Topup(w, req)
	})
	r.mux.HandleFunc("/api/payment/transactions", func(w http.ResponseWriter, req *http.Request) {
		r.payH.ListTransactions(w, req)
	})
	r.mux.HandleFunc("/api/payment/simulate-fleet", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.payH.SimulateFleetPayments(w, req)
	})

	// Trajectory API
	r.mux.HandleFunc("/api/agent/trajectory", func(w http.ResponseWriter, req *http.Request) {
		r.payH.GetTrajectory(w, req)
	})
	r.mux.HandleFunc("/api/agent/trajectory/simulate", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		r.payH.SimulateTrajectory(w, req)
	})

	// Autonomous Band Agent API (Single Endpoint)
	r.mux.Handle("/api/agent/v1", r.agentH)
	r.mux.Handle("/api/agent/v1/", r.agentH)
	r.mux.Handle("/api/agent", r.agentH)
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

	r.mux.HandleFunc("/control/seed-ten", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			WriteError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		if err := r.store.SeedDefaultRestaurants(req.Context()); err != nil {
			WriteError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		rests, _ := r.store.ListRestaurants(req.Context())
		WriteJSON(w, http.StatusOK, map[string]interface{}{"status": "ok", "seeded": len(rests)})
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
