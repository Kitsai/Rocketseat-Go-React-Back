package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"

	"github.com/Kitsai/Rocketseat-Go-React-Back/internal/store/pgstore"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

type apiHandler struct {
	q           *pgstore.Queries
	r           *chi.Mux
	upgrader    websocket.Upgrader
	subscribers map[string]map[*websocket.Conn]context.CancelFunc
	mu          *sync.Mutex
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}

func NewHandler(q *pgstore.Queries) http.Handler {
	a := apiHandler{
		q:           q,
		upgrader:    websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
		subscribers: make(map[string]map[*websocket.Conn]context.CancelFunc),
		mu:          &sync.Mutex{},
	}

	r := chi.NewRouter()

	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300,
	}))

	r.Get("/subscribe/{room_id}", a.handleSubscribe)

	r.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Post("/", a.handleCreateRoom)
			r.Get("/", a.handleGetRooms)

			r.Route("/{room_id}/messages", func(r chi.Router) {
				r.Post("/", a.handleCreateRoomMessage)
				r.Get("/", a.handleGetRoomMessages)

				r.Route("/{message_id}", func(r chi.Router) {
					r.Get("/", a.handleGetRoomMessage)
					r.Patch("/react", a.handleReactToMessage)
					r.Delete("/react", a.handleRemoveReactFromMessage)
					r.Patch("/answer", a.handleMarkMessageAsAnswered)
				})
			})
		})
	})
	
	a.r = r
	return a
}

const (
	MessageKindMessageCreated          = "message_created"
	MessageKindReactedToMessage        = "reacted_to_message"
	MessageKindRemovedReactFromMessage = "removed_reaction_from_message"
	MessageKindMarkMessageAsAnswered   = "marked_message_as_answered"
)

type MessageReactedToMessage struct {
	ID    string `json:"id"`
	Value int64  `json:"value"`
}
type MessageRemovedReactFromMessage struct {
	ID    string `json:"id"`
	Value int64  `json:"value"`
}
type MessageMessageCreated struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

type MessageMarkMessageAsAnswered struct {
	ID string `json:"id"`
}

type Message struct {
	Kind   string `json:"kind"`
	Value  any    `json:"value"`
	RoomID string `json:"-"`
}

func (h apiHandler) notifyClients(msg Message) {
	h.mu.Lock()
	defer h.mu.Unlock()

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
}

func (h apiHandler) getPathID(w http.ResponseWriter, r *http.Request, v string) (string, uuid.UUID, error) {
	rawPathID := chi.URLParam(r, v)
	pathID, err := uuid.Parse(rawPathID)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid %s id", v), http.StatusBadRequest)
		return "", uuid.UUID{}, err
	}

	switch v {
		case "room_id":
			_, err = h.q.GetRoom(r.Context(), pathID) 
		case "message_id":
			_, err = h.q.GetMessage(r.Context(), pathID)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, fmt.Sprintf("%s not found", v), http.StatusBadRequest)
			return "", uuid.UUID{}, err
		}

		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return "", uuid.UUID{}, err
	}

	return rawPathID, pathID, nil
}
func (h apiHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {

	rawRoomID, _, err := h.getPathID(w, r, "room_id")
	if err != nil {
		return
	}

	c, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("failed to upgrade connection", "error", err)
		http.Error(w, "failed to upgrade to ws connection", http.StatusBadRequest)
		return
	}

	defer c.Close()

	ctx, cancel := context.WithCancel(r.Context())
	h.mu.Lock()
	if _, ok := h.subscribers[rawRoomID]; !ok {
		h.subscribers[rawRoomID] = make(map[*websocket.Conn]context.CancelFunc)
	}
	slog.Info("new client connected", "room_id", rawRoomID, "client_ip", r.RemoteAddr)
	h.subscribers[rawRoomID][c] = cancel
	h.mu.Unlock()

	<-ctx.Done()

	h.mu.Lock()
	delete(h.subscribers[rawRoomID], c)
	h.mu.Unlock()
}
func (h apiHandler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	type _body struct {
		Theme string `json:"theme"`
	}
	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	roomID, err := h.q.InsertRoom(r.Context(), body.Theme)
	if err != nil {
		slog.Error("failed to insert room", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: roomID.String()})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}
func (h apiHandler) handleGetRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := h.q.GetRooms(r.Context())
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	data, _ := json.Marshal(rooms)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)

}
func (h apiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {
	_, roomID, err := h.getPathID(w, r, "room_id")
	if err != nil {
		return
	}

	messages, err := h.q.GetRoomMessages(r.Context(), roomID)
	if err != nil {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	data, _ := json.Marshal(messages)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}
func (h apiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request) {
	_, roomID, err := h.getPathID(w, r, "room_id")
	if err != nil {
		return
	}
	_, messageID, err := h.getPathID(w, r, "message_id")
	if err != nil {
		return
	}
	message, err := h.q.GetMessage(r.Context(), messageID)
	if message.RoomID != roomID {
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	data, _ := json.Marshal(message)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}
func (h apiHandler) handleCreateRoomMessage(w http.ResponseWriter, r *http.Request) {
	rawRoomID, roomID, err := h.getPathID(w, r, "room_id")
	if err != nil {
		return
	}

	type _body struct {
		Message string `json:"message"`
	}
	var body _body
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	messageID, err := h.q.InsertMessage(r.Context(), pgstore.InsertMessageParams{RoomID: roomID, Message: body.Message})
	if err != nil {
		slog.Error("failed to insert message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: messageID.String()})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)

	go h.notifyClients(Message{
		Kind:   MessageKindMessageCreated,
		RoomID: rawRoomID,
		Value: MessageMessageCreated{
			ID:      messageID.String(),
			Message: body.Message,
		},
	})
}
func (h apiHandler) handleReactToMessage(w http.ResponseWriter, r *http.Request) {
	rawRoomID, _, err := h.getPathID(w, r, "room_id")
	if err != nil {
		return
	}
	rawMessageID, messageID, err := h.getPathID(w, r, "message_id")
	if err != nil {
		return
	}
	value, err := h.q.ReactToMessage(r.Context(), messageID)
	if err != nil {
		slog.Error("failed to react to message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		Value int64 `json:"value"`
	}
	data, _ := json.Marshal(response{Value: value})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)

	go h.notifyClients(Message{
		Kind:   MessageKindReactedToMessage,
		RoomID: rawRoomID,
		Value: MessageReactedToMessage{
			ID:    rawMessageID,
			Value: value,
		},
	})
}
func (h apiHandler) handleRemoveReactFromMessage(w http.ResponseWriter, r *http.Request) {
	rawRoomID, _, err := h.getPathID(w, r, "room_id")
	if err != nil {
		return
	}
	rawMessageID, messageID, err := h.getPathID(w, r, "message_id")
	if err != nil {
		return
	}
	value, err := h.q.RemoveReactionFromMessage(r.Context(), messageID)
	if err != nil {
		slog.Error("failed to remove react to message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type response struct {
		Value int64 `json:"value"`
	}
	data, _ := json.Marshal(response{Value: value})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)

	go h.notifyClients(Message{
		Kind:   MessageKindRemovedReactFromMessage,
		RoomID: rawRoomID,
		Value: MessageRemovedReactFromMessage{
			ID:    rawMessageID,
			Value: value,
		},
	})
}
func (h apiHandler) handleMarkMessageAsAnswered(w http.ResponseWriter, r *http.Request) {
	rawRoomID, _, err := h.getPathID(w, r, "room_id")
	if err != nil {
		return
	}
	rawMessageID, messageID, err := h.getPathID(w, r, "message_id")
	if err != nil {
		return
	}
	if err := h.q.MarkMessagedAsAnswered(r.Context(), messageID); err != nil {
		slog.Error("failed to mark message as answered", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	go h.notifyClients(Message{
		Kind:   MessageKindMarkMessageAsAnswered,
		RoomID: rawRoomID,
		Value: MessageMarkMessageAsAnswered{
			ID: rawMessageID,
		},
	})
}
