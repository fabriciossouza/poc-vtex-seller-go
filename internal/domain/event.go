package domain

type EventType string

const EventOrderAuthorized EventType = "order.authorized"

type Event struct {
	Type    EventType
	OrderID string
}
