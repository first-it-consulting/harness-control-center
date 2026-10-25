package control

import "slices"

func sortJobs(jobs []JobView) {
	slices.SortStableFunc(jobs, func(a, b JobView) int { return b.Created.Compare(a.Created) })
}
