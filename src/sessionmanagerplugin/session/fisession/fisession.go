package fisession

import "github.com/fibanez/session-manager-plugin/src/sessionmanagerplugin/session/portsession"

func CreateNewPortPlugin() *PortSession {
	return &portsession.PortSession{}
}
