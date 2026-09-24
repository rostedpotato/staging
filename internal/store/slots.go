package store

// HiddenSlots returns the set of slots hidden by admins (in-app setting).
func (s *Store) HiddenSlots() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT slot FROM slot_settings WHERE hidden = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var slot string
		if err := rows.Scan(&slot); err != nil {
			return nil, err
		}
		out[slot] = true
	}
	return out, rows.Err()
}

// SetSlotHidden upserts the hidden flag for a slot.
func (s *Store) SetSlotHidden(slot string, hidden bool) error {
	h := 0
	if hidden {
		h = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO slot_settings (slot, hidden) VALUES (?, ?)
		ON CONFLICT(slot) DO UPDATE SET hidden = excluded.hidden`, slot, h)
	return err
}
