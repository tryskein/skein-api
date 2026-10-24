package users

import "skein-api/internal/eventbus"

// eventbusActor copies a users.Actor into the package-neutral
// eventbus.Actor shape. Field-for-field — no derived defaults — so the
// boundary between users and eventbus is purely syntactic.
func eventbusActor(a Actor) eventbus.Actor {
	return eventbus.Actor{
		UserID:    a.UserID,
		Username:  a.Username,
		Role:      a.Role,
		SessionID: a.SessionID,
	}
}
