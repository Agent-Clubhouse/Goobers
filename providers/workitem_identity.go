package providers

import "strconv"

func positiveNativeID(id int64) string {
	if id <= 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func githubAssigneeLogins(users []githubUser) []string {
	logins := make([]string, 0, len(users))
	for _, user := range users {
		if user.Login != "" {
			logins = append(logins, user.Login)
		}
	}
	return logins
}

func adoNativeAssignees(fields map[string]interface{}) []string {
	if aliases := identityAliases(fields, "System.AssignedTo"); len(aliases) > 0 {
		return aliases[:1]
	}
	if display := stringField(fields, "System.AssignedTo"); display != "" {
		return []string{display}
	}
	return nil
}
