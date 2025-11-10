package level

// Выбор нужной роли по уровню
func (r *Registry) roleForLevel(level int) (want string, toRemove []string) {
	switch {
	case level >= 100:
		return r.roleL100Plus, []string{r.roleL75to99, r.roleL50to74, r.roleL25to49, r.roleL1to24}
	case level >= 75:
		return r.roleL75to99, []string{r.roleL50to74, r.roleL25to49, r.roleL1to24, r.roleL100Plus}
	case level >= 50:
		return r.roleL50to74, []string{r.roleL25to49, r.roleL1to24, r.roleL75to99, r.roleL100Plus}
	case level >= 25:
		return r.roleL25to49, []string{r.roleL1to24, r.roleL50to74, r.roleL75to99, r.roleL100Plus}
	default:
		return r.roleL1to24, []string{r.roleL25to49, r.roleL50to74, r.roleL75to99, r.roleL100Plus}
	}
}

// Выдать нужную роль и снять лишние
func (r *Registry) applyLevelRoles(userID string, level int) error {
	want, rm := r.roleForLevel(level)

	mem, err := r.s.GuildMember(r.GuildID, userID)
	if err != nil {
		return err
	}

	has := func(roleID string) bool {
		for _, x := range mem.Roles {
			if x == roleID {
				return true
			}
		}
		return false
	}

	if want != "" && !has(want) {
		_ = r.s.GuildMemberRoleAdd(r.GuildID, userID, want)
	}
	for _, rid := range rm {
		if rid != "" && has(rid) {
			_ = r.s.GuildMemberRoleRemove(r.GuildID, userID, rid)
		}
	}
	return nil
}
