package projects

import "skein-api/internal/eventbus"

// eventbusActor copies a projects.Actor into the package-neutral
// eventbus.Actor shape. Field-for-field — no derived defaults — so the
// boundary between projects and eventbus is purely syntactic.
func eventbusActor(a Actor) eventbus.Actor {
	return eventbus.Actor{
		UserID:    a.UserID,
		Username:  a.Username,
		Role:      a.Role,
		SessionID: a.SessionID,
	}
}
