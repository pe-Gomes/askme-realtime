package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/go-playground/validator/v10"
	"github.com/goccy/go-json"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/pe-Gomes/askme-realtime/internal/store/pgstore"
)

type apiHandler struct {
	q           *pgstore.Queries
	r           *chi.Mux
	v           *validator.Validate
	upgrader    websocket.Upgrader
	subscribers map[string]map[*websocket.Conn]context.CancelFunc
	mutex       *sync.Mutex
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewHandler(q *pgstore.Queries) http.Handler {
	validator := validator.New(validator.WithRequiredStructEnabled())

	a := &apiHandler{
		q: q,
		v: validator,
		upgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool {
			return true
		}},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		mutex:       &sync.Mutex{},
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept, Authorization, Content-Type, X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/subscribe/{roomID}", a.handleSubscribeToRoom)

	r.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Post("/", a.handleCreateRoom)
			r.Get("/", a.handleGetRooms)

			r.Route("/{roomID}/messages", func(r chi.Router) {
				r.Post("/", a.handleCreateRoomMessage)
				r.Get("/", a.handleGetRoomMessages)

				r.Route("/{messageID}", func(r chi.Router) {
					r.Get("/", a.handleGetRoomMessage)

					r.Patch("/react", a.handleReactToRoomMessage)
					r.Delete("/react", a.handleRemoveReactionFromRoomMessage)

					r.Post("/answer", a.handleCreateMessageAnswer)
					r.Patch("/answer", a.handleMarkMessageAnswered)
				})
			})
		})
		r.Route("/messages/{messageID}", func(r chi.Router) {
			r.Get("/", a.handleGetMessageAnswers)
		})
	})
	a.r = r

	return a
}

const (
	MessageKindMessageCreated  = "message_created"
	MessageKindMessageReaction = "message_reacted"
	MessageKindMessageAnswered = "message_answered"
	MessageKindMessageAnswer   = "message_answer"
)

type MessageMessageCreated struct {
	ID      string
	Message string
}

type MessageMessageReaction struct {
	ID            string
	ReactionCount int64
}

type MessageMessageAnswered struct {
	ID       string
	Answered bool
}

type MessageMessageAnswer struct {
	ID     string
	Answer string
}

type Message struct {
	Kind   string `json:"kind"`
	Value  any    `json:"value"`
	RoomID string `json:"-"`
}

func (h apiHandler) notifyClient(msg Message) {
	h.mutex.Lock()

	subscribers, ok := h.subscribers[msg.RoomID]
	if !ok || len(subscribers) == 0 {
		return
	}

	for conn, cancel := range subscribers {
		if err := conn.WriteJSON(msg); err != nil {
			slog.Error("failed to send message to client", "error", err)
			cancel()
		}
	}
	h.mutex.Unlock()
}

func (h apiHandler) handleSubscribeToRoom(w http.ResponseWriter, r *http.Request) {
	rawRoomID := chi.URLParam(r, "roomID")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room ID", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	c, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("failed to upgrade connection", "error", err)
		http.Error(w, "failed to upgrade to websocket connection", http.StatusBadRequest)
		return
	}

	defer c.Close()

	ctx, cancel := context.WithCancel(r.Context())

	h.mutex.Lock()
	if _, ok := h.subscribers[rawRoomID]; !ok {
		h.subscribers[rawRoomID] = make(map[*websocket.Conn]context.CancelFunc)
	}
	slog.Info("new client connected", "roomID", rawRoomID, "clientIP", r.RemoteAddr)
	h.subscribers[rawRoomID][c] = cancel
	h.mutex.Unlock()

	<-ctx.Done()

	h.mutex.Lock()
	delete(h.subscribers[rawRoomID], c)
	h.mutex.Unlock()
}

func (h apiHandler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}

	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	roomID, err := h.q.CreateRoom(r.Context(), body.Theme)
	if err != nil {
		slog.Error("failed to create room", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: roomID.String()})

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (h apiHandler) handleGetRooms(w http.ResponseWriter, r *http.Request) {}

func (h apiHandler) handleCreateRoomMessage(w http.ResponseWriter, r *http.Request) {
	rawRoomID := chi.URLParam(r, "roomID")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room ID", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		http.Error(w, "room not found", http.StatusBadRequest)
		return
	}

	type _body struct {
		Message string `json:"message" validate:"required,min=2,max=255"`
	}

	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
	}

	if err := h.v.Struct(body); err != nil {
		http.Error(w, "invalid input: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}

	messageID, err := h.q.InsertMessage(r.Context(), pgstore.InsertMessageParams{
		RoomID:  roomID,
		Message: body.Message,
	})
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: messageID.String()})

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)

	go h.notifyClient(Message{
		Kind:   MessageKindMessageCreated,
		RoomID: rawRoomID,
		Value: MessageMessageCreated{
			ID:      messageID.String(),
			Message: body.Message,
		},
	})
}

func (h apiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {
	rawRoomID := chi.URLParam(r, "roomID")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room ID", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		http.Error(w, "room not found", http.StatusBadRequest)
		return
	}

	messages, err := h.q.GetRoomMessages(r.Context(), roomID)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
	}

	type _innerResponse struct {
		ID            string    `json:"id"`
		RoomID        string    `json:"room_id"`
		Message       string    `json:"message"`
		ReactionCount int64     `json:"reaction_count"`
		Answered      bool      `json:"answered"`
		CreatedAt     time.Time `json:"created_at"`
	}

	type _response struct {
		Messages []_innerResponse `json:"messages"`
	}

	innerResponse := make([]_innerResponse, len(messages))

	for i, msg := range messages {
		innerResponse[i] = _innerResponse{
			ID:            msg.ID.String(),
			RoomID:        roomID.String(),
			Message:       msg.Message,
			ReactionCount: msg.ReactionCount,
			Answered:      msg.Answered,
			CreatedAt:     msg.CreatedAt.Time,
		}
	}

	data, _ := json.Marshal(_response{
		Messages: innerResponse,
	})

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (h apiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {
	rawRoomID := chi.URLParam(r, "roomID")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room ID", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	rawMessageID := chi.URLParam(r, "messageID")
	messageID, err := uuid.Parse(rawMessageID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	msg, err := h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type _response struct {
		ID            string    `json:"id"`
		RoomID        string    `json:"room_id"`
		Message       string    `json:"message"`
		ReactionCount int64     `json:"reaction_count"`
		Answered      bool      `json:"answered"`
		CreatedAt     time.Time `json:"created_at"`
	}

	data, _ := json.Marshal(_response{
		ID:            msg.ID.String(),
		RoomID:        msg.RoomID.String(),
		Message:       msg.Message,
		ReactionCount: msg.ReactionCount,
		Answered:      msg.Answered,
		CreatedAt:     msg.CreatedAt.Time,
	})

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (h apiHandler) handleReactToRoomMessage(w http.ResponseWriter, r *http.Request) {
	rawRoomID := chi.URLParam(r, "roomID")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room ID", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	rawMessageID := chi.URLParam(r, "messageID")
	messageID, err := uuid.Parse(rawMessageID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	reactions, err := h.q.ReactToMessage(r.Context(), messageID)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type _response struct {
		ReactionCount int64 `json:"reaction_count"`
	}

	data, _ := json.Marshal(_response{
		ReactionCount: reactions,
	})

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)

	go h.notifyClient(Message{
		Kind:   MessageKindMessageReaction,
		RoomID: rawRoomID,
		Value: MessageMessageReaction{
			ID:            messageID.String(),
			ReactionCount: reactions,
		},
	})
}

func (h apiHandler) handleRemoveReactionFromRoomMessage(w http.ResponseWriter, r *http.Request) {
	rawRoomID := chi.URLParam(r, "roomID")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room ID", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	rawMessageID := chi.URLParam(r, "messageID")
	messageID, err := uuid.Parse(rawMessageID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	reactions, err := h.q.RemoveReactionToMessage(r.Context(), messageID)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type _response struct {
		ReactionCount int64 `json:"reaction_count"`
	}

	data, _ := json.Marshal(_response{
		ReactionCount: reactions,
	})

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)

	go h.notifyClient(Message{
		Kind:   MessageKindMessageReaction,
		RoomID: rawRoomID,
		Value: MessageMessageReaction{
			ID:            messageID.String(),
			ReactionCount: reactions,
		},
	})
}

func (h apiHandler) handleCreateMessageAnswer(w http.ResponseWriter, r *http.Request) {
	rawRoomID := chi.URLParam(r, "roomID")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room ID", http.StatusBadRequest)
		return
	}

	rawMessageID := chi.URLParam(r, "messageID")
	messageID, err := uuid.Parse(rawMessageID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}
	_, err = h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	type _body struct {
		Answer string `json:"answer" validate:"required,min=2"`
	}
	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
	}
	if err := h.v.Struct(body); err != nil {
		http.Error(w, "invalid input: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	answerID, err := h.q.InsertMessageAnswer(r.Context(), pgstore.InsertMessageAnswerParams{
		MessageID: messageID,
		Answer:    body.Answer,
	})
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	type response struct {
		ID string `json:"id"`
	}
	data, _ := json.Marshal(response{ID: answerID.String()})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)

	go h.notifyClient(Message{
		Kind: MessageKindMessageAnswer,
		Value: MessageMessageAnswer{
			ID:     answerID.String(),
			Answer: body.Answer,
		},
		RoomID: roomID.String(),
	})
}

func (h apiHandler) handleGetMessageAnswers(w http.ResponseWriter, r *http.Request) {
	rawMessageID := chi.URLParam(r, "messageID")
	messageID, err := uuid.Parse(rawMessageID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}
	_, err = h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	answers, err := h.q.GetMessageAnswers(r.Context(), messageID)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
	}
	type _innerResponse struct {
		ID        string    `json:"id"`
		MessageID string    `json:"message_id"`
		Answer    string    `json:"answer"`
		CreatedAt time.Time `json:"created_at"`
	}
	type _response struct {
		Answers []_innerResponse `json:"answers"`
	}
	innerResponse := make([]_innerResponse, len(answers))
	for i, ans := range answers {
		innerResponse[i] = _innerResponse{
			ID:        ans.ID.String(),
			MessageID: messageID.String(),
			Answer:    ans.Answer,
			CreatedAt: ans.CreatedAt.Time,
		}
	}
	data, _ := json.Marshal(_response{
		Answers: innerResponse,
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

func (h apiHandler) handleMarkMessageAnswered(w http.ResponseWriter, r *http.Request) {
	rawRoomID := chi.URLParam(r, "roomID")
	roomID, err := uuid.Parse(rawRoomID)
	if err != nil {
		http.Error(w, "invalid room ID", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	rawMessageID := chi.URLParam(r, "messageID")
	messageID, err := uuid.Parse(rawMessageID)
	if err != nil {
		http.Error(w, "invalid message id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetMessage(r.Context(), messageID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "message not found", http.StatusBadRequest)
			return
		}
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	if err := h.q.MarkMessageAsAnswered(r.Context(), messageID); err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)

	go h.notifyClient(Message{
		Kind:   MessageKindMessageAnswered,
		RoomID: rawRoomID,
		Value: MessageMessageAnswered{
			ID:       messageID.String(),
			Answered: true,
		},
	})
}
