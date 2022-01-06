package fisession

import "session-manager-plugin/src/sessionmanagerplugin/session/portsession"

func CreateNewPortPlugin() *PortSession {
	return &portsession.PortSession{}
}
