package main

import (
	"encoding/json"
	"log"
	"time"
)

type AuditLogger struct{}

func NewAuditLogger() *AuditLogger {
	return &AuditLogger{}
}

func (a *AuditLogger) Log(entry AuditEntry) {
	entry.Timestamp = time.Now()
	data, err := json.Marshal(entry)
	if err != nil {
		log.Printf("[AUDIT ERROR] %v", err)
		return
	}
	log.Printf("[AUDIT] %s", string(data))
}
