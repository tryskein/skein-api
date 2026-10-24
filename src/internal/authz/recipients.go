package authz

import "skein-api/internal/authz/authzsql"

// RecipientVisibleExpr — see authzsql.RecipientVisibleExpr.
func RecipientVisibleExpr(userExpr, projectExpr string) string {
	return authzsql.RecipientVisibleExpr(userExpr, projectExpr)
}

// RecipientEligibleExpr — see authzsql.RecipientEligibleExpr.
func RecipientEligibleExpr(userExpr, projectExpr string) string {
	return authzsql.RecipientEligibleExpr(userExpr, projectExpr)
}

// WorkerEligibleExpr — see authzsql.WorkerEligibleExpr.
func WorkerEligibleExpr(userExpr, projectExpr string) string {
	return authzsql.WorkerEligibleExpr(userExpr, projectExpr)
}

// VisibleNowSetExpr — see authzsql.VisibleNowSetExpr.
func VisibleNowSetExpr(userExpr, projectExpr string) string {
	return authzsql.VisibleNowSetExpr(userExpr, projectExpr)
}
