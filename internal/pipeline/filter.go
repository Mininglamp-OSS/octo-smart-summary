package pipeline

// FilterWithContext keeps target users' messages plus N context messages before/after each.
func FilterWithContext(messages []Message, targetUIDs []string, contextWindow int) []Message {
	if contextWindow < 0 {
		contextWindow = 0
	}
	if len(targetUIDs) == 0 {
		return nil
	}

	targetSet := make(map[string]bool, len(targetUIDs))
	for _, uid := range targetUIDs {
		targetSet[uid] = true
	}

	var targetIndices []int
	for i, m := range messages {
		if targetSet[m.SenderUID] {
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
			m.IsTargetUser = targetSet[m.SenderUID]
			result = append(result, m)
		}
	}
	return result
}
