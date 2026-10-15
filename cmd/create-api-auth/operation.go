package main

import "github.com/asdf57/stigmergy/internal/api"

func operationPermissions() []api.AccessPermission {
	return []api.AccessPermission{
		{Kind: "Operation", Methods: []string{"GET", "POST"}},
		{Kind: "Operation", Subresource: "status", Methods: []string{"PATCH"}},
	}
}
