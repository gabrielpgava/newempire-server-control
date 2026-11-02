package model

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"time"
)

var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func ValidID(id string) bool { return idPattern.MatchString(id) }

type Instance struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Root             string    `json:"root"`
	JavaPath         string    `json:"java_path"`
	Jar              string    `json:"jar"`
	MinecraftVersion string    `json:"minecraft_version"`
	LoaderVersion    string    `json:"loader_version"`
	MemoryMB         int       `json:"memory_mb"`
	Port             int       `json:"port"`
	RCONPort         int       `json:"rcon_port"`
	RCONSecret       string    `json:"-"`
	EULA             bool      `json:"eula"`
	Adopted          bool      `json:"adopted"`
	CreatedAt        time.Time `json:"created_at"`
}

type RuntimeStatus struct {
	State         string   `json:"state"`
	Ready         bool     `json:"ready"`
	PID           int      `json:"pid,omitempty"`
	UptimeSeconds float64  `json:"uptime_seconds"`
	MemoryBytes   uint64   `json:"memory_bytes"`
	CPUSeconds    float64  `json:"cpu_seconds"`
	DiskFreeBytes *uint64  `json:"disk_free_bytes,omitempty"`
	Players       []string `json:"players"`
	Detail        string   `json:"detail,omitempty"`
}

type LogLine struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}
