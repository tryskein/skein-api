package issues

import "skein-api/internal/eventbus"

// eventbusActor copies an issues.Actor into the package-neutral
// eventbus.Actor shape so eventbus.WriteHybridUpdateEvents can stay free
// of any cross-package import (an eventbus → issues import would create a
// cycle: issues already depends on eventbus). Field-for-field copy — no
// transformation, no derived defaults — keeps the boundary obvious.
func eventbusActor(a Actor) eventbus.Actor {
	return eventbus.Actor{
		UserID:    a.UserID,
		Username:  a.Username,
		Role:      a.Role,
		SessionID: a.SessionID,
	}
}
