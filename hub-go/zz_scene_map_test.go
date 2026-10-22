package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestZZSceneMap(t *testing.T) {
	dir := domCredDirs()["CodeBuddy凭证"]
	p := NewDomProxyDir(dir)
	cm := NewCredentialManager(p.AuthFilePath(), p.Backend, p.DefaultDomain, p.UserAgent)
	h, _ := cm.Headers()
	b, _, _ := httpGetJSON(p.Backend+"/v2/as/support/scenes", h)
	var r struct {
		Data struct {
			Scenes []struct {
				ID      json.Number `json:"id"`
				Name    string      `json:"name"`
				Mode    string      `json:"mode"`
				SceneID string      `json:"sceneId"`
				Title   string      `json:"title"`
			} `json:"scenes"`
		} `json:"data"`
	}
	json.Unmarshal(gunzip(b), &r)
	fmt.Printf("共 %d 个场景:\n", len(r.Data.Scenes))
	for _, s := range r.Data.Scenes {
		fmt.Printf("  id=%-12s mode=%-8s %s\n", s.ID, s.Mode, s.Name)
	}
}
