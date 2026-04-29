package pipeline

// FilterWithContext keeps target user's messages plus N context messages before/after each
func FilterWithContext(messages []Message, userID string, contextWindow int) []Message {
	if contextWindow < 0 {
		contextWindow = 0
	}

	var targetIndices []int
	for i, m := range messages {
		if m.SenderUID == userID {
			targetIndices = append(targetIndices, i)
		}
	}

	if len(targetIndices) == 0 {
		return nil
	}

	keep := make(map[int]bool)
	for _, idx := range targetIndices {
		for j := idx - contextWindow; j <= idx+contextWindow; j++ {
			if j >= 0 && j < len(messages) {
				keep[j] = true
			}
		}
	}

	var result []Message
	for i, m := range messages {
		if keep[i] {
			m.IsTargetUser = (m.SenderUID == userID)
			result = append(result, m)
		}
	}
	return result
}
