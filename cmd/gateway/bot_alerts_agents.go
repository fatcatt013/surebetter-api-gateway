package main

// alertAttachLocked: codeID's bot socket opened (name = its code's name). b.mu must be held.
func (b *botHub) alertAttachLocked(userID, codeID int64, name string, fx *effects) {
	if b.alerts == nil {
		return
	}
	if name != "" {
		b.alerts.names[codeID] = name
	}
}
