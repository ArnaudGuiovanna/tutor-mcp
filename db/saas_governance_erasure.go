// Copyright (c) 2026 Arnaud Guiovanna
// SPDX-License-Identifier: MIT

package db

import "context"

// erasedLearnerCohorts lists the published institutional cohorts whose
// aggregates include the learner, as (cohort, version) pairs.
func (s *Store) erasedLearnerCohorts(ctx context.Context, tenant, learner string) ([][2]string, error) {
	rows, err := s.query(ctx, `SELECT DISTINCT c.id, c.formation_version_id FROM enrollments e
 JOIN cohorts c ON c.tenant_id = e.tenant_id AND c.id = e.cohort_id
 JOIN formation_versions v ON v.tenant_id = c.tenant_id AND v.id = c.formation_version_id
 JOIN formations f ON f.tenant_id = v.tenant_id AND f.id = v.formation_id
 WHERE e.tenant_id = ? AND e.learner_id = ? AND v.status = 'published' AND `+institutionalFormationSQL, tenant, learner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		out = append(out, pair)
	}
	return out, rows.Err()
}
