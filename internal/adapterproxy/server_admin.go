package adapterproxy

import "sort"

// SessionInitiatorMappings exposes learned session->initiator identity for
// status/admin surfaces.
func (server *Server) SessionInitiatorMappings() []SessionInitiatorMapping {
	server.mutex.Lock()
	mappings := make([]SessionInitiatorMapping, 0, len(server.learnedBySession))
	for sessionID, learning := range server.learnedBySession {
		mappings = append(mappings, SessionInitiatorMapping{
			SessionID: sessionID,
			Initiator: learning.Initiator,
			LearnedAt: learning.LearnedAt,
			Source:    learning.Source,
		})
	}
	server.mutex.Unlock()

	sort.Slice(mappings, func(i, j int) bool {
		return mappings[i].SessionID < mappings[j].SessionID
	})
	return mappings
}
