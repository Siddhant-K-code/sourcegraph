package insights

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/inconshreveable/log15"
	"github.com/keegancsmith/sqlf"
	"github.com/lib/pq"
	"github.com/segmentio/ksuid"

	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/database/basestore"
	"github.com/sourcegraph/sourcegraph/internal/oobmigration"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

const schemaErrorPrefix = "insights oob migration schema error"

func NewMigrator(insightsDB, postgresDB database.DB) oobmigration.Migrator {
	return &migrator{
		frontendStore: basestore.NewWithHandle(postgresDB.Handle()),
		insightsStore: basestore.NewWithHandle(insightsDB.Handle()),
	}
}

func (m *migrator) Progress(ctx context.Context) (float64, error) {
	progress, _, err := basestore.ScanFirstFloat(m.frontendStore.Query(ctx, sqlf.Sprintf(insightsMigratorProgressQuery)))
	return progress, err
}

const insightsMigratorProgressQuery = `
-- source: enterprise/internal/oobmigration/migrations/insights/migration.go:Progress
SELECT
	CASE c2.count WHEN 0 THEN 1 ELSE
		CAST(c1.count AS FLOAT) / CAST(c2.count AS FLOAT)
	END
FROM
	(SELECT COUNT(*) AS count FROM insights_settings_migration_jobs WHERE completed_at IS NOT NULL) c1,
	(SELECT COUNT(*) AS count FROM insights_settings_migration_jobs) c2
`

func (m *migrator) Up(ctx context.Context) (err error) {
	globalMigrationComplete, err := m.performBatchMigration(ctx, GlobalJob)
	if err != nil {
		return err
	}
	if !globalMigrationComplete {
		return nil
	}

	orgMigrationComplete, err := m.performBatchMigration(ctx, OrgJob)
	if err != nil {
		return err
	}
	if !orgMigrationComplete {
		return nil
	}

	userMigrationComplete, err := m.performBatchMigration(ctx, UserJob)
	if err != nil {
		return err
	}
	if !userMigrationComplete {
		return nil
	}

	return nil
}

func (m *migrator) Down(ctx context.Context) (err error) {
	return nil
}

func (m *migrator) performBatchMigration(ctx context.Context, jobType settingsMigrationJobType) (bool, error) {
	// This transaction will allow us to lock the jobs rows while working on them.
	tx, err := m.frontendStore.Transact(ctx)
	if err != nil {
		return false, err
	}
	defer func() {
		err = tx.Done(err)
	}()

	var cond *sqlf.Query
	switch jobType {
	case "USER":
		cond = sqlf.Sprintf("user_id IS NOT NULL")
	case "ORG":
		cond = sqlf.Sprintf("org_id IS NOT NULL")
	default:
		cond = sqlf.Sprintf("global IS TRUE")
	}
	count, _, err := basestore.ScanFirstInt(tx.Query(ctx, sqlf.Sprintf(`
		SELECT COUNT(*) FROM insights_settings_migration_jobs WHERE %s AND completed_at IS NULL
	`, cond)))
	if err != nil {
		return false, err
	}
	if count == 0 {
		return true, nil
	}

	jobs, err := scanJobs(tx.Query(ctx, sqlf.Sprintf(`
	SELECT
		user_id,
		org_id,
		(CASE WHEN global IS NULL THEN FALSE ELSE TRUE END) AS global,
		migrated_insights,
		migrated_dashboards,
		runs
	FROM insights_settings_migration_jobs
	WHERE
		%s AND
		completed_at IS NULL
	LIMIT 100
	FOR UPDATE SKIP LOCKED
	`, cond)))
	if err != nil {
		return false, err
	}
	if len(jobs) == 0 {
		return false, nil
	}

	var errs error
	for _, job := range jobs {
		err := m.performMigrationForRow(ctx, tx, job)
		if err != nil {
			errs = errors.Append(errs, err)
		}
	}

	// We'll rely on the next thread to return true right away, if everything has completed.
	return false, errs
}

func (m *migrator) performMigrationForRow(ctx context.Context, tx *basestore.Store, job settingsMigrationJob) error {
	var subject settingsSubject
	var migrationContext migrationContext
	var subjectName string

	defer func() {
		var cond *sqlf.Query
		if job.UserId != nil {
			cond = sqlf.Sprintf("user_id = %s", *job.UserId)
		} else if job.OrgId != nil {
			cond = sqlf.Sprintf("org_id = %s", *job.OrgId)
		} else {
			cond = sqlf.Sprintf("global IS TRUE")
		}
		tx.Exec(ctx, sqlf.Sprintf(`UPDATE insights_settings_migration_jobs SET runs = %s WHERE %s`, job.Runs+1, cond))
	}()

	if job.UserId != nil {
		userId := int32(*job.UserId)
		subject = settingsSubject{User: &userId}

		// when this is a user setting we need to load all of the organizations the user is a member of so that we can
		// resolve insight ID collisions as if it were in a setting cascade
		orgs, err := scanOrgs(tx.Query(ctx, sqlf.Sprintf(`
			SELECT
				orgs.id,
				orgs.name,
				orgs.display_name,
				orgs.created_at,
				orgs.updated_at
			FROM org_members
			LEFT OUTER JOIN orgs ON org_members.org_id = orgs.id
			WHERE
				user_id = %s AND
				orgs.deleted_at IS NULL
		`,
			userId,
		)))
		if err != nil {
			return err
		}
		orgIds := make([]int, 0, len(orgs))
		for _, org := range orgs {
			orgIds = append(orgIds, int(org.ID))
		}
		migrationContext.userId = int(userId)
		migrationContext.orgIds = orgIds

		users, err := scanUsers(tx.Query(ctx, sqlf.Sprintf(`
			SELECT
				u.id,
				u.username,
				u.display_name,
				u.avatar_url,
				u.created_at,
				u.updated_at,
				u.site_admin,
				u.passwd IS NOT NULL,
				u.tags,
				u.invalidated_sessions_at,
				u.tos_accepted,
				u.searchable
			FROM users u
			WHERE
				id = %s AND
				deleted_at IS NULL
			LIMIT 1
		`,
			userId,
		)))
		if err != nil {
			return errors.Wrap(err, "UserStoreGetByID")
		}
		if len(users) == 0 {
			// If the user doesn't exist, just mark the job complete.
			err = tx.Exec(ctx, sqlf.Sprintf(`UPDATE insights_settings_migration_jobs SET completed_at = NOW() WHERE user_id = %s`, userId))
			if err != nil {
				return errors.Wrap(err, "MarkCompleted")
			}
			return nil
		}
		user := users[0]
		subjectName = replaceIfEmpty(&user.DisplayName, user.Username)
	} else if job.OrgId != nil {
		orgId := int32(*job.OrgId)
		subject = settingsSubject{Org: &orgId}
		migrationContext.orgIds = []int{*job.OrgId}
		orgs, err := scanOrgs(tx.Query(ctx, sqlf.Sprintf(`
			SELECT
				id,
				name,
				display_name,
				created_at,
				updated_at
			FROM orgs
			WHERE
				deleted_at IS NULL AND
				id = %s
			LIMIT 1
		`, orgId,
		)))
		if err != nil {
			return errors.Wrap(err, "OrgStoreGetByID")
		}
		if len(orgs) == 0 {
			// If the org doesn't exist, just mark the job complete.
			err = tx.Exec(ctx, sqlf.Sprintf(`UPDATE insights_settings_migration_jobs SET completed_at = NOW() WHERE org_id = %s`, orgId))
			if err != nil {
				return errors.Wrap(err, "MarkCompleted")
			}
			return nil
		}
		org := orgs[0]
		subjectName = replaceIfEmpty(org.DisplayName, org.Name)
	} else {
		subject = settingsSubject{Site: true}
		// nothing to set for migration context, it will infer global based on the lack of user / orgs
		subjectName = "Global"
	}

	var cond *sqlf.Query
	switch {
	case subject.Org != nil:
		cond = sqlf.Sprintf("org_id = %d", *subject.Org)
	case subject.User != nil:
		cond = sqlf.Sprintf("user_id = %d AND EXISTS (SELECT NULL FROM users WHERE id=%d AND deleted_at IS NULL)", *subject.User, *subject.User)
	default:
		// No org and no user represents global site settings.
		cond = sqlf.Sprintf("user_id IS NULL AND org_id IS NULL")
	}
	settings, err := scanSettings(tx.Query(ctx, sqlf.Sprintf(`
		SELECT
			s.id,
			s.org_id,
			s.user_id,
			CASE WHEN users.deleted_at IS NULL THEN s.author_user_id ELSE NULL END,
			s.contents,
			s.created_at
		FROM settings s
		LEFT JOIN users ON users.id = s.author_user_id
		WHERE %s
		ORDER BY id DESC LIMIT 1
		`,
		cond,
	)))
	if err != nil {
		return err
	}
	if len(settings) == 0 {
		// If this settings object no longer exists, skip it.
		return nil
	}

	langStatsInsights := getLangStatsInsights(settings[0])
	frontendInsights := getFrontendInsights(settings[0])
	backendInsights := getBackendInsights(settings[0])

	// here we are constructing a total set of all of the insights defined in this specific settings block. This will help guide us
	// to understand which insights are created here, versus which are referenced from elsewhere. This will be useful for example
	// to reconstruct the special case user / org / global dashboard
	allDefinedInsightIds := make([]string, 0, len(langStatsInsights)+len(frontendInsights)+len(backendInsights))
	for _, insight := range langStatsInsights {
		allDefinedInsightIds = append(allDefinedInsightIds, insight.ID)
	}
	for _, insight := range frontendInsights {
		allDefinedInsightIds = append(allDefinedInsightIds, insight.ID)
	}
	for _, insight := range backendInsights {
		allDefinedInsightIds = append(allDefinedInsightIds, insight.ID)
	}
	logDuplicates(allDefinedInsightIds)

	if job.UserId != nil {
		cond = sqlf.Sprintf("user_id = %s", *job.UserId)
	} else if job.OrgId != nil {
		cond = sqlf.Sprintf("org_id = %s", *job.OrgId)
	} else {
		cond = sqlf.Sprintf("global IS TRUE")
	}

	totalInsights := len(langStatsInsights) + len(frontendInsights) + len(backendInsights)
	var migratedInsightsCount int
	var insightMigrationErrors error
	if totalInsights != job.MigratedInsights {
		err = tx.Exec(ctx, sqlf.Sprintf(`UPDATE insights_settings_migration_jobs SET total_insights = %s WHERE %s`, totalInsights, cond))
		if err != nil {
			return err
		}

		count, err := m.migrateLangStatsInsights(ctx, langStatsInsights)
		insightMigrationErrors = errors.Append(insightMigrationErrors, err)
		migratedInsightsCount += count

		count, err = m.migrateInsights(ctx, frontendInsights, frontend)
		insightMigrationErrors = errors.Append(insightMigrationErrors, err)
		migratedInsightsCount += count

		count, err = m.migrateInsights(ctx, backendInsights, backend)
		insightMigrationErrors = errors.Append(insightMigrationErrors, err)
		migratedInsightsCount += count

		err = tx.Exec(ctx, sqlf.Sprintf(`UPDATE insights_settings_migration_jobs SET migrated_insights = %s WHERE %s`, migratedInsightsCount, cond))
		if err != nil {
			return errors.Append(insightMigrationErrors, err)
		}
		if totalInsights != migratedInsightsCount {
			return insightMigrationErrors
		}
	}

	dashboards := getDashboards(settings[0])
	totalDashboards := len(dashboards)
	if totalDashboards != job.MigratedDashboards {
		err = tx.Exec(ctx, sqlf.Sprintf(`UPDATE insights_settings_migration_jobs SET total_dashboards = %s WHERE %s`, totalDashboards, cond))
		if err != nil {
			return err
		}
		migratedDashboardsCount, dashboardMigrationErrors := m.migrateDashboards(ctx, dashboards, migrationContext)
		err = tx.Exec(ctx, sqlf.Sprintf(`UPDATE insights_settings_migration_jobs SET migrated_dashboards = %s WHERE %s`, migratedDashboardsCount, cond))
		if err != nil {
			return err
		}
		if totalDashboards != migratedDashboardsCount {
			return dashboardMigrationErrors
		}
	}
	err = m.createSpecialCaseDashboard(ctx, subjectName, allDefinedInsightIds, migrationContext)
	if err != nil {
		return err
	}

	if job.UserId != nil {
		cond = sqlf.Sprintf("user_id = %s", *job.UserId)
	} else if job.OrgId != nil {
		cond = sqlf.Sprintf("org_id = %s", *job.OrgId)
	} else {
		cond = sqlf.Sprintf("global IS TRUE")
	}
	err = tx.Exec(ctx, sqlf.Sprintf(`UPDATE insights_settings_migration_jobs SET completed_at = NOW() WHERE %s`, cond))
	if err != nil {
		return errors.Wrap(err, "MarkCompleted")
	}

	return nil
}

func (m *migrator) migrateLangStatsInsights(ctx context.Context, toMigrate []langStatsInsight) (int, error) {
	var count int
	var errs error
	for _, d := range toMigrate {
		if d.ID == "" {
			// we need a unique ID, and if for some reason this insight doesn't have one, it can't be migrated.
			// since it can never be migrated, we count it towards the total
			log15.Error(schemaErrorPrefix, "owner", getOwnerNameFromLangStatsInsight(d), "error msg", "insight failed to migrate due to missing id")
			count++
			continue
		}
		numInsights, _, err := basestore.ScanFirstInt(m.insightsStore.Query(ctx, sqlf.Sprintf(`
			SELECT COUNT(*)
			FROM (
				SELECT *
				FROM insight_view
				WHERE unique_id = %s
				ORDER BY unique_id
			) iv
			JOIN insight_view_series ivs ON iv.id = ivs.insight_view_id
			JOIN insight_series i ON ivs.insight_series_id = i.id
			WHERE i.deleted_at IS NULL
		`,
			d.ID,
		)))
		if err != nil {
			errs = errors.Append(errs, err)
			continue
		}
		if numInsights > 0 {
			// this insight has already been migrated, so count it towards the total
			count++
			continue
		}

		err = migrateLangStatSeries(ctx, m.insightsStore, d)
		if err != nil {
			errs = errors.Append(errs, err)
			continue
		} else {
			count++
		}
	}
	return count, errs
}

func migrateLangStatSeries(ctx context.Context, insightStore *basestore.Store, from langStatsInsight) (err error) {
	tx, err := insightStore.Transact(ctx)
	if err != nil {
		return err
	}
	defer func() { err = tx.Done(err) }()

	now := time.Now()
	view := insightView{
		Title:            from.Title,
		UniqueID:         from.ID,
		OtherThreshold:   &from.OtherThreshold,
		PresentationType: Pie,
	}
	series := insightSeries{
		SeriesID:           ksuid.New().String(),
		Repositories:       []string{from.Repository},
		SampleIntervalUnit: string(Month),
		JustInTime:         true,
		GenerationMethod:   LanguageStats,
		CreatedAt:          now,
	}
	interval := TimeInterval{
		Unit:  intervalUnit(series.SampleIntervalUnit),
		Value: series.SampleIntervalValue,
	}
	validType := false
	switch interval.Unit {
	case Year:
		fallthrough
	case Month:
		fallthrough
	case Week:
		fallthrough
	case Day:
		fallthrough
	case Hour:
		validType = true
	}
	if !(validType && interval.Value >= 0) {
		interval = TimeInterval{
			Unit:  Month,
			Value: 1,
		}
	}

	if series.NextRecordingAfter.IsZero() {
		series.NextRecordingAfter = interval.StepForwards(now)
	}
	if series.NextSnapshotAfter.IsZero() {
		series.NextSnapshotAfter = nextSnapshot(now)
	}
	if series.OldestHistoricalAt.IsZero() {
		// TODO(insights): this value should probably somewhere more discoverable / obvious than here
		series.OldestHistoricalAt = now.Add(-time.Hour * 24 * 7 * 26)
	}
	var grants []insightViewGrant
	if from.UserID != nil {
		grants = []insightViewGrant{userGrant(int(*from.UserID))}
	} else if from.OrgID != nil {
		grants = []insightViewGrant{orgGrant(int(*from.OrgID))}
	} else {
		grants = []insightViewGrant{globalGrant()}
	}

	viewID, _, err := basestore.ScanFirstInt(tx.Query(ctx, sqlf.Sprintf(`
	INSERT INTO insight_view (
		title,
		description,
		unique_id,
		default_filter_include_repo_regex,
		default_filter_exclude_repo_regex,
		default_filter_search_contexts,
		other_threshold,
		presentation_type,
	)
	VALUES (%s, %s, %s, %s, %s, %s, %s, %s)
	RETURNING id
	`,
		view.Title,
		view.Description,
		view.UniqueID,
		view.Filters.IncludeRepoRegex,
		view.Filters.ExcludeRepoRegex,
		pq.Array(view.Filters.SearchContexts),
		view.OtherThreshold,
		view.PresentationType,
	)))
	if err != nil {
		return errors.Wrapf(err, "unable to migrate insight view, unique_id: %s", from.ID)
	}
	view.ID = viewID
	values := make([]*sqlf.Query, 0, len(grants))
	for _, grant := range grants {
		values = append(values, sqlf.Sprintf("(%s, %s, %s, %s)", view.ID, grant.OrgID, grant.UserID, grant.Global))
	}
	err = tx.Exec(ctx, sqlf.Sprintf(`INSERT INTO insight_view_grants (insight_view_id, org_id, user_id, global) VALUES %s`, sqlf.Join(values, ", ")))
	if err != nil {
		return errors.Wrapf(err, "unable to migrate insight view, unique_id: %s", from.ID)
	}

	seriesID, _, err := basestore.ScanFirstInt(tx.Query(ctx, sqlf.Sprintf(`
			INSERT INTO insight_series (
				series_id,
				query,
				created_at,
				oldest_historical_at,
				last_recorded_at,
				next_recording_after,
				last_snapshot_at,
				next_snapshot_after
				repositories,
				sample_interval_unit,
				sample_interval_value,
				generated_from_capture_groups,
				just_in_time,
				generation_method,
				group_by,
				needs_migration,
			)
			VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, false)
			RETURNING id
		`,
		series.SeriesID,
		series.Query,
		series.CreatedAt,
		series.OldestHistoricalAt,
		series.LastRecordedAt,
		series.NextRecordingAfter,
		series.LastSnapshotAt,
		series.NextSnapshotAfter,
		pq.Array(series.Repositories),
		series.SampleIntervalUnit,
		series.SampleIntervalValue,
		series.GeneratedFromCaptureGroups,
		series.JustInTime,
		series.GenerationMethod,
		series.GroupBy,
	)))
	if err != nil {
		return errors.Wrapf(err, "unable to migrate insight series, unique_id: %s", from.ID)
	}
	series.ID = seriesID
	series.Enabled = true

	metadata := insightViewSeriesMetadata{}
	err = tx.Exec(ctx, sqlf.Sprintf(`
		INSERT INTO insight_view_series (
			insight_series_id,
			insight_view_id,
			label,
			stroke
		)
		VALUES (%s, %s, %s, %s)
	`,
		series.ID,
		view.ID,
		metadata.Label,
		metadata.Stroke,
	))
	if err != nil {
		return err
	}
	// Enable the series in case it had previously been soft-deleted.
	err = tx.Exec(ctx, sqlf.Sprintf(`
		UPDATE insight_series
		SET deleted_at IS NULL
		WHERE series_id = %s
	`,
		series.SeriesID,
	))

	return nil
}

func (m *migrator) migrateInsights(ctx context.Context, toMigrate []searchInsight, batch migrationBatch) (int, error) {
	var count int
	var errs error
	for _, d := range toMigrate {
		if d.ID == "" {
			// we need a unique ID, and if for some reason this insight doesn't have one, it can't be migrated.
			// skippable error
			count++
			log15.Error(schemaErrorPrefix, "owner", getOwnerNameFromInsight(d), "error msg", "insight failed to migrate due to missing id")
			continue
		}

		numInsights, _, err := basestore.ScanFirstInt(m.insightsStore.Query(ctx, sqlf.Sprintf(`
			SELECT COUNT(*)
			FROM (
				SELECT *
				FROM insight_view
				WHERE unique_id = %s
				ORDER BY unique_id
			) iv
			JOIN insight_view_series ivs ON iv.id = ivs.insight_view_id
			JOIN insight_series i ON ivs.insight_series_id = i.id
			WHERE i.deleted_at IS NULL
		`,
			d.ID,
		)))
		if err != nil {
			errs = errors.Append(errs, err)
			continue
		}
		if numInsights > 0 {
			// this insight has already been migrated, so count it
			count++
			continue
		}
		err = migrateSeries(ctx, m.insightsStore, m.frontendStore, d, batch)
		if err != nil {
			errs = errors.Append(errs, err)
			continue
		} else {
			count++
		}
	}
	return count, errs
}

func migrateSeries(ctx context.Context, insightStore *basestore.Store, workerStore *basestore.Store, from searchInsight, batch migrationBatch) (err error) {
	tx, err := insightStore.Transact(ctx)
	if err != nil {
		return err
	}
	defer func() { err = tx.Done(err) }()

	dataSeries := make([]insightSeries, len(from.Series))
	metadata := make([]insightViewSeriesMetadata, len(from.Series))

	for i, timeSeries := range from.Series {
		temp := insightSeries{
			Query: timeSeries.Query,
		}

		if batch == frontend {
			temp.Repositories = from.Repositories
			if temp.Repositories == nil {
				// this shouldn't be possible, but if for some reason we get here there is a malformed schema
				// we can't do anything to fix this, so skip this insight
				log15.Error(schemaErrorPrefix, "owner", getOwnerNameFromInsight(from), "error msg", "insight failed to migrate due to missing repositories")
				return nil
			}
			interval := parseTimeInterval(from)
			temp.SampleIntervalUnit = string(interval.unit)
			temp.SampleIntervalValue = interval.value
			temp.SeriesID = ksuid.New().String() // this will cause some orphan records, but we can't use the query to match because of repo / time scope. We will purge orphan records at the end of this job.
			temp.JustInTime = true
			temp.GenerationMethod = Search
		} else if batch == backend {
			temp.SampleIntervalUnit = string(Month)
			temp.SampleIntervalValue = 1
			temp.NextRecordingAfter = nextRecording(time.Now())
			temp.NextSnapshotAfter = nextSnapshot(time.Now())
			temp.SeriesID = ksuid.New().String()
			temp.JustInTime = false
			temp.GenerationMethod = Search
		}
		now := time.Now()

		if temp.CreatedAt.IsZero() {
			temp.CreatedAt = now
		}
		interval := TimeInterval{
			Unit:  intervalUnit(temp.SampleIntervalUnit),
			Value: temp.SampleIntervalValue,
		}
		validType := false
		switch interval.Unit {
		case Year:
			fallthrough
		case Month:
			fallthrough
		case Week:
			fallthrough
		case Day:
			fallthrough
		case Hour:
			validType = true
		}
		if !(validType && interval.Value >= 0) {
			interval = TimeInterval{
				Unit:  Month,
				Value: 1,
			}
		}

		if temp.NextRecordingAfter.IsZero() {
			temp.NextRecordingAfter = interval.StepForwards(now)
		}
		if temp.NextSnapshotAfter.IsZero() {
			temp.NextSnapshotAfter = nextSnapshot(now)
		}
		if temp.OldestHistoricalAt.IsZero() {
			// TODO(insights): this value should probably somewhere more discoverable / obvious than here
			temp.OldestHistoricalAt = now.Add(-time.Hour * 24 * 7 * 26)
		}

		var series insightSeries

		// Backend series require special consideration to re-use series
		if batch == backend {
			rows, err := scanSeries(tx.Query(ctx, sqlf.Sprintf(`
				SELECT
					id,
					series_id,
					query,
					created_at,
					oldest_historical_at,
					last_recorded_at,
					next_recording_after,
					last_snapshot_at,
					next_snapshot_after,
					(CASE WHEN deleted_at IS NULL THEN TRUE ELSE FALSE END) AS enabled,
					sample_interval_unit,
					sample_interval_value,
					generated_from_capture_groups,
					just_in_time,
					generation_method,
					repositories,
					group_by,
					backfill_attempts
				FROM insight_series
				WHERE
					(repositories = '{}' OR repositories is NULL) AND
					query = %s AND
					sample_interval_unit = %s AND
					sample_interval_value = %s AND
					generated_from_capture_groups = %s AND
					group_by IS NULL
			`,
				temp.Query,
				temp.SampleIntervalUnit,
				temp.SampleIntervalValue,
				false,
			)))
			if err != nil {
				return errors.Wrapf(err, "unable to migrate insight unique_id: %s series_id: %s", from.ID, temp.SeriesID)
			}
			if len(rows) > 0 {
				// If the series already exists, we can re-use that series
				series = rows[0]
			} else {

				// If it's not a backend series, we just want to create it.
				id, _, err := basestore.ScanFirstInt(tx.Query(ctx, sqlf.Sprintf(`
				INSERT INTO insight_series (
					series_id,
					query,
					created_at,
					oldest_historical_at,
					last_recorded_at,
					next_recording_after,
					last_snapshot_at,
					next_snapshot_after,
					repositories,
					sample_interval_unit,
					sample_interval_value,
					generated_from_capture_groups,
					just_in_time,
					generation_method,
					group_by,
					needs_migration
					)
				VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, false)
				RETURNING id
			`,
					temp.SeriesID,
					temp.Query,
					temp.CreatedAt,
					temp.OldestHistoricalAt,
					temp.LastRecordedAt,
					temp.NextRecordingAfter,
					temp.LastSnapshotAt,
					temp.NextSnapshotAfter,
					pq.Array(temp.Repositories),
					temp.SampleIntervalUnit,
					temp.SampleIntervalValue,
					temp.GeneratedFromCaptureGroups,
					temp.JustInTime,
					temp.GenerationMethod,
					temp.GroupBy,
				)))
				if err != nil {
					return errors.Wrapf(err, "unable to migrate insight unique_id: %s series_id: %s", from.ID, temp.SeriesID)
				}
				temp.ID = id
				temp.Enabled = true
				series = temp

				// Also match/replace old series_points ids with the new series id
				oldId := fmt.Sprintf("s:%s", fmt.Sprintf("%X", sha256.Sum256([]byte(timeSeries.Query))))
				countUpdated, _, silentErr := basestore.ScanFirstInt(tx.Query(ctx, sqlf.Sprintf(`
						WITH updated AS (
							UPDATE series_points sp
							SET series_id = %s
							WHERE series_id = %s
							RETURNING sp.series_id
						)
						SELECT count(*) FROM updated;
					`,
					temp.SeriesID,
					oldId,
				)))
				if silentErr != nil {
					// If the find-replace fails, it's not a big deal. It will just need to be calcuated again.
					log15.Error("error updating series_id for series_points", "series_id", temp.SeriesID, "err", silentErr)
				} else if countUpdated == 0 {
					// If find-replace doesn't match any records, we still need to backfill, so just continue
				} else {
					// If the find-replace succeeded, we can do a similar find-replace on the jobs in the queue,
					// and then stamp the backfill_queued_at on the new series.

					if err := workerStore.Exec(ctx, sqlf.Sprintf("update insights_query_runner_jobs set series_id = %s where series_id = %s", temp.SeriesID, oldId)); err != nil {
						// If the find-replace fails, it's not a big deal. It will just need to be calcuated again.
						log15.Error("error updating series_id for jobs", "series_id", temp.SeriesID, "err", errors.Wrap(err, "updateTimeSeriesJobReferences"))
					} else {
						now := time.Now()
						silentErr := tx.Exec(ctx, sqlf.Sprintf(`UPDATE insight_series SET backfill_queued_at = %s WHERE id = %s`, now, series.ID))
						series.BackfillQueuedAt = now
						if silentErr != nil {
							// If the stamp fails, skip it. It will just need to be calcuated again.
							log15.Error("error updating backfill_queued_at", "series_id", temp.SeriesID, "err", silentErr)
						}
					}
				}
			}
		} else {

			// If it's not a backend series, we just want to create it.
			id, _, err := basestore.ScanFirstInt(tx.Query(ctx, sqlf.Sprintf(`
				INSERT INTO insight_series (
					series_id,
					query,
					created_at,
					oldest_historical_at,
					last_recorded_at,
					next_recording_after,
					last_snapshot_at,
					next_snapshot_after,
					repositories,
					sample_interval_unit,
					sample_interval_value,
					generated_from_capture_groups,
					just_in_time,
					generation_method,
					group_by,
					needs_migration
					)
				VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, false)
				RETURNING id
			`,
				temp.SeriesID,
				temp.Query,
				temp.CreatedAt,
				temp.OldestHistoricalAt,
				temp.LastRecordedAt,
				temp.NextRecordingAfter,
				temp.LastSnapshotAt,
				temp.NextSnapshotAfter,
				pq.Array(temp.Repositories),
				temp.SampleIntervalUnit,
				temp.SampleIntervalValue,
				temp.GeneratedFromCaptureGroups,
				temp.JustInTime,
				temp.GenerationMethod,
				temp.GroupBy,
			)))
			if err != nil {
				return errors.Wrapf(err, "unable to migrate insight unique_id: %s series_id: %s", from.ID, temp.SeriesID)
			}
			temp.ID = id
			temp.Enabled = true
			series = temp
		}
		dataSeries[i] = series

		metadata[i] = insightViewSeriesMetadata{
			Label:  timeSeries.Name,
			Stroke: timeSeries.Stroke,
		}
	}

	view := insightView{
		Title:            from.Title,
		Description:      from.Description,
		UniqueID:         from.ID,
		PresentationType: Line,
	}

	if from.Filters != nil {
		view.Filters = insightViewFilters{
			IncludeRepoRegex: from.Filters.IncludeRepoRegexp,
			ExcludeRepoRegex: from.Filters.ExcludeRepoRegexp,
		}
	}

	var grants []insightViewGrant
	if from.UserID != nil {
		grants = []insightViewGrant{userGrant(int(*from.UserID))}
	} else if from.OrgID != nil {
		grants = []insightViewGrant{orgGrant(int(*from.OrgID))}
	} else {
		grants = []insightViewGrant{globalGrant()}
	}

	viewID, _, err := basestore.ScanFirstInt(tx.Query(ctx, sqlf.Sprintf(`
		INSERT INTO insight_view (
			title,
			description,
			unique_id,
			default_filter_include_repo_regex,
			default_filter_exclude_repo_regex,
			default_filter_search_contexts,
			other_threshold,
			presentation_type,
		)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s)
		RETURNING id
	`,
		view.Title,
		view.Description,
		view.UniqueID,
		view.Filters.IncludeRepoRegex,
		view.Filters.ExcludeRepoRegex,
		pq.Array(view.Filters.SearchContexts),
		view.OtherThreshold,
		view.PresentationType,
	)))
	view.ID = viewID

	values := make([]*sqlf.Query, 0, len(grants))
	for _, grant := range grants {
		values = append(values, sqlf.Sprintf("(%s, %s, %s, %s)", view.ID, grant.OrgID, grant.UserID, grant.Global))
	}
	err = tx.Exec(ctx, sqlf.Sprintf(`INSERT INTO insight_view_grants (insight_view_id, org_id, user_id, global) VALUES %s`, sqlf.Join(values, ", ")))
	if err != nil {
		return err
	}

	for i, insightSeries := range dataSeries {
		err = tx.Exec(ctx, sqlf.Sprintf(
			`INSERT INTO insight_view_series (
				insight_series_id,
				insight_view_id,
				label,
				stroke,
			)
			VALUES (%s, %s, %s, %s)
		`,
			insightSeries.ID,
			view.ID,
			metadata[i].Label,
			metadata[i].Stroke,
		))
		if err != nil {
			return err
		}

		err = tx.Exec(ctx, sqlf.Sprintf(`UPDATE insight_series SET deleted_at IS NULL WHERE series_id = %s`, insightSeries.SeriesID))
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *migrator) migrateDashboards(ctx context.Context, toMigrate []settingDashboard, mc migrationContext) (int, error) {
	var count int
	var errs error
	for _, d := range toMigrate {
		if d.ID == "" {
			// we need a unique ID, and if for some reason this insight doesn't have one, it can't be migrated.
			// since it can never be migrated, we count it towards the total
			log15.Error(schemaErrorPrefix, "owner", getOwnerNameFromDashboard(d), "error msg", "dashboard failed to migrate due to missing id")
			count++
			continue
		}
		err := m.migrateDashboard(ctx, d, mc)
		if err != nil {
			errs = errors.Append(errs, err)
		} else {
			count++
		}
	}
	return count, errs
}

func (m *migrator) migrateDashboard(ctx context.Context, from settingDashboard, migrationContext migrationContext) (err error) {
	tx, err := m.insightsStore.Transact(ctx)
	if err != nil {
		return err
	}
	defer func() { err = tx.Done(err) }()

	var grantsQuery *sqlf.Query
	if from.UserID != nil {
		grantsQuery = sqlf.Sprintf("dg.user_id = %s", *from.UserID)
	} else if from.OrgID != nil {
		grantsQuery = sqlf.Sprintf("dg.org_id = %s", *from.OrgID)
	} else {
		grantsQuery = sqlf.Sprintf("dg.global IS TRUE")
	}

	count, _, err := basestore.ScanFirstInt(tx.Query(ctx, sqlf.Sprintf(`
		SELECT COUNT(*) from dashboard
		JOIN dashboard_grants dg ON dashboard.id = dg.dashboard_id
		WHERE
			dashboard.title = %s AND
			%s
	`,
		from.Title,
		grantsQuery,
	)))
	if err != nil {
		return err
	}
	if count != 0 {
		return nil
	}

	err = m.createDashboard(ctx, tx, from.Title, from.InsightIds, migrationContext)
	if err != nil {
		return err
	}

	return nil
}

func (m *migrator) createSpecialCaseDashboard(ctx context.Context, subjectName string, insightReferences []string, migration migrationContext) error {
	tx, err := m.insightsStore.Transact(ctx)
	if err != nil {
		return err
	}
	defer func() { err = tx.Done(err) }()

	err = m.createDashboard(ctx, tx, specialCaseDashboardTitle(subjectName), insightReferences, migration)
	if err != nil {
		return errors.Wrap(err, "CreateSpecialCaseDashboard")
	}
	return nil
}

func (m *migrator) createDashboard(ctx context.Context, tx *basestore.Store, title string, insightReferences []string, migration migrationContext) (err error) {
	var mapped []string

	for _, reference := range insightReferences {
		id, _, err := basestore.ScanFirstString(m.insightsStore.Query(ctx, migration.ToInsightUniqueIdQuery(reference)))
		if err != nil {
			return err
		}
		mapped = append(mapped, id)
	}

	dashboardId, _, err := basestore.ScanFirstInt(tx.Query(ctx, sqlf.Sprintf(`
		INSERT INTO dashboard (title, save, type)
		VALUES (%s, %s, %s)
		RETURNING id
	`,
		title,
		true,
		standard,
	)))
	if len(mapped) > 0 {
		// Create rows for an inline table which is used to preserve the ordering of the viewIds.
		orderings := make([]*sqlf.Query, 0, 1)
		for i, viewId := range mapped {
			orderings = append(orderings, sqlf.Sprintf("(%s, %s)", viewId, fmt.Sprintf("%d", i)))
		}

		err = tx.Exec(ctx, sqlf.Sprintf(`
			INSERT INTO dashboard_insight_view (dashboard_id, insight_view_id)
			SELECT %s AS dashboard_id, insight_view.id AS insight_view_id
			FROM insight_view
			JOIN (VALUES %s) as ids (id, ordering) ON ids.id = insight_view.unique_id
			WHERE unique_id = ANY(%s)
			ORDER BY ids.ordering
			ON CONFLICT DO NOTHING;
		`,
			dashboardId,
			sqlf.Join(orderings, ","),
			pq.Array(mapped),
		))
		if err != nil {
			return errors.Wrap(err, "AddViewsToDashboard")
		}
	}

	if migration.userId != 0 {
		if err := tx.Exec(ctx, sqlf.Sprintf(`INSERT INTO dashboard_grants (dashboard_id, user_id) VALUES (%s, %s)`, dashboardId, migration.userId)); err != nil {
			return errors.Wrap(err, "AddDashboardGrants")
		}
	} else if len(migration.orgIds) == 1 {
		if err := tx.Exec(ctx, sqlf.Sprintf(`INSERT INTO dashboard_grants (dashboard_id, org_id) VALUES (%s, %s)`, dashboardId, migration.orgIds[0])); err != nil {
			return errors.Wrap(err, "AddDashboardGrants")
		}
	} else {
		if err := tx.Exec(ctx, sqlf.Sprintf(`INSERT INTO dashboard_grants (dashboard_id, global) VALUES (%s, %s)`, dashboardId, true)); err != nil {
			return errors.Wrap(err, "AddDashboardGrants")
		}
	}

	return nil
}
