package main

import "strings"

// accountPrefix is the stable, human-visible form of an account identity:
// "<website-id>/<account-id>". Account IDs are only unique inside a website,
// so every place that needs to name one account across the deployment uses
// this key instead of the bare ID.
const accountKeySeparator = "/"

func accountKey(websiteID, accountID string) string {
	if websiteID == "" {
		return accountID
	}
	return websiteID + accountKeySeparator + accountID
}

// splitAccountKey reverses accountKey. A key without a separator is a bare
// account ID with no website, which legacy configs may still contain.
func splitAccountKey(key string) (websiteID, accountID string) {
	if index := strings.Index(key, accountKeySeparator); index >= 0 {
		return key[:index], key[index+1:]
	}
	return "", key
}

// accountIndexMatches counts how many accounts share a bare ID across all
// websites, which tells the admin API whether a bare ID is unambiguous.
func accountIndexMatches(accounts []AccountConfig, accountID string) int {
	count := 0
	for _, account := range accounts {
		if account.ID == accountID {
			count++
		}
	}
	return count
}
