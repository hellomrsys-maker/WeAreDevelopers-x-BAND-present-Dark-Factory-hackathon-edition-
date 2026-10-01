package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"tablekeeper/contracts"
	"tablekeeper/store"

	"golang.org/x/crypto/bcrypt"
)

var emailRegex = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func generateToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func generateUserID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("u_%x", b), nil
}

type AuthHandler struct {
	store *store.Store
}

func NewAuthHandler(s *store.Store) *AuthHandler {
	return &AuthHandler{store: s}
}

type SignupRequest struct {
	Email       *string `json:"email"`
	Password    *string `json:"password"`
	DisplayName *string `json:"display_name"`
}

func (h *AuthHandler) Signup(w http.ResponseWriter, r *http.Request) {
	var req SignupRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "malformed_request", "unparseable body or invalid field types")
		return
	}

	if req.Email == nil || req.Password == nil || req.DisplayName == nil {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "email, password, and display_name are required")
		return
	}

	email := strings.TrimSpace(*req.Email)
	password := *req.Password
	displayName := strings.TrimSpace(*req.DisplayName)

	if !emailRegex.MatchString(email) {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid email format")
		return
	}
	if len(password) < 8 {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "password must be at least 8 characters")
		return
	}
	if displayName == "" {
		WriteError(w, http.StatusUnprocessableEntity, "validation_failed", "display_name cannot be empty")
		return
	}

	// Check email uniqueness
	existing, _ := h.store.GetUserByEmail(r.Context(), email)
	if existing != nil {
		WriteError(w, http.StatusConflict, "email_taken", "email already registered")
		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to hash password")
		return
	}

	userID, err := generateUserID()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to generate user id")
		return
	}

	newUser := &contracts.User{
		ID:          userID,
		Email:       email,
		Password:    string(hashedPassword),
		DisplayName: displayName,
	}

	if err := h.store.CreateUser(r.Context(), newUser); err != nil {
		WriteError(w, http.StatusConflict, "email_taken", "email already registered")
		return
	}

	token, err := generateToken()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to generate token")
		return
	}
	if err := h.store.CreateToken(r.Context(), token, userID); err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to save token")
		return
	}

	WriteJSON(w, http.StatusCreated, map[string]interface{}{
		"user_id":      userID,
		"display_name": displayName,
		"token":        token,
	})
}

type LoginRequest struct {
	Email    *string `json:"email"`
	Password *string `json:"password"`
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req LoginRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		WriteError(w, http.StatusBadRequest, "malformed_request", "unparseable body or invalid field types")
		return
	}

	if req.Email == nil || req.Password == nil {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "email and password are required")
		return
	}

	user, err := h.store.GetUserByEmail(r.Context(), *req.Email)
	if err != nil || user == nil {
		WriteError(w, http.StatusUnauthorized, "unauthenticated", "invalid email or password")
		return
	}

	// Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(*req.Password)); err != nil {
		// Also support plaintext seeded passwords fallback
		if user.Password != *req.Password {
			WriteError(w, http.StatusUnauthorized, "unauthenticated", "invalid email or password")
			return
		}
	}

	token, err := generateToken()
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to generate token")
		return
	}
	if err := h.store.CreateToken(r.Context(), token, user.ID); err != nil {
		WriteError(w, http.StatusInternalServerError, "internal_error", "failed to save token")
		return
	}

	WriteJSON(w, http.StatusOK, map[string]interface{}{
		"user_id":      user.ID,
		"display_name": user.DisplayName,
		"token":        token,
	})
}
