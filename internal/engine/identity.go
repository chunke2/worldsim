package engine

import "fmt"

// ResolveIdentity is read-only. Legacy worlds are accepted only with one explicit role.
func ResolveIdentity(s *WorldState) (hero, pov string, err error) {
	hero = s.WorldLevel.Protagonist
	if hero == "" {
		for name, e := range s.Entities {
			if name != "" && e.Extra["role"] == "protagonist" {
				if hero != "" {
					return "", "", fmt.Errorf("多个主角标记，请显式设置 world_level.protagonist")
				}
				hero = name
			}
		}
	}
	if _, ok := s.Entities[hero]; hero == "" || !ok {
		return "", "", fmt.Errorf("缺少有效主角，请设置 world_level.protagonist；不会随机选择角色")
	}
	pov = s.WorldLevel.NarrativePOV
	if pov == "" {
		pov = hero
	}
	if _, ok := s.Entities[pov]; !ok {
		return "", "", fmt.Errorf("叙事视角角色不存在: %s", pov)
	}
	return hero, pov, nil
}
