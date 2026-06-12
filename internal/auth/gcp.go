package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// GCPCredential persists an uploaded GCP credential (service account key or
// authorized_user JSON) together with the project/region it applies to.
type GCPCredential struct {
	ProjectID   string          `json:"project_id"`
	Region      string          `json:"region"`
	Credentials json.RawMessage `json:"credentials"`
}

const gcpCredFile = "vertex-gcp.json"

func LoadGCPCredential(dir string) *GCPCredential {
	data, err := os.ReadFile(filepath.Join(dir, gcpCredFile))
	if err != nil {
		return nil
	}
	var c GCPCredential
	if err := json.Unmarshal(data, &c); err != nil || len(c.Credentials) == 0 {
		return nil
	}
	return &c
}

func SaveGCPCredential(dir string, c *GCPCredential) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, gcpCredFile), data, 0600)
}

func DeleteGCPCredential(dir string) error {
	err := os.Remove(filepath.Join(dir, gcpCredFile))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
