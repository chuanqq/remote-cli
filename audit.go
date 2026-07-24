package main

import (
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"
)

type AuditLogger struct{}

func NewAuditLogger() *AuditLogger {
	return &AuditLogger{}
}

func (a *AuditLogger) Log(entry AuditEntry) {
	entry.Timestamp = time.Now()
	// Guarantee every entry is correlatable: REST stream executions and file
	// ops have no natural execution ID, so mint one when missing.
	if entry.RequestID == "" {
		entry.RequestID = "audit-" + uuid.New().String()[:8]
	}
	// Never persist secrets (passwords, tokens) to the log.
	entry.Command = RedactCommand(entry.Command)
	data, err := json.Marshal(entry)
	if err != nil {
		log.Printf("[AUDIT ERROR] %v", err)
		return
	}
	log.Printf("[AUDIT] %s", string(data))
}
