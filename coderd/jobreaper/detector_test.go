package jobreaper_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"cdr.dev/slog"
	"github.com/coder/coder/v2/coderd/coderdtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbauthz"
	"github.com/coder/coder/v2/coderd/database/dbgen"
	"github.com/coder/coder/v2/coderd/database/dbtestutil"
	"github.com/coder/coder/v2/coderd/jobreaper"
	"github.com/coder/coder/v2/coderd/provisionerdserver"
	"github.com/coder/coder/v2/coderd/rbac"
	"github.com/coder/coder/v2/provisionersdk"
	"github.com/coder/coder/v2/testutil"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, testutil.GoleakOptions...)
}

func TestDetectorNoJobs(t *testing.T) {
	t.Parallel()

	var (
		ctx        = testutil.Context(t, testutil.WaitLong)
		db, pubsub = dbtestutil.NewDB(t)
		log        = testutil.Logger(t)
		tickCh     = make(chan time.Time)
		statsCh    = make(chan jobreaper.Stats)
	)

	detector := jobreaper.New(ctx, wrapDBAuthz(db, log), pubsub, log, tickCh).WithStatsChannel(statsCh)
	detector.Start()
	tickCh <- time.Now()

	stats := <-statsCh
	require.NoError(t, stats.Error)
	require.Empty(t, stats.TerminatedJobIDs)

	detector.Close()
	detector.Wait()
}

func TestDetectorNoHungJobs(t *testing.T) {
	t.Parallel()

	var (
		ctx        = testutil.Context(t, testutil.WaitLong)
		db, pubsub = dbtestutil.NewDB(t)
		log        = testutil.Logger(t)
		tickCh     = make(chan time.Time)
		statsCh    = make(chan jobreaper.Stats)
	)

	// Insert some jobs that are running and haven't been updated in a while,
	// but not enough to be considered hung.
	now := time.Now()
	org := dbgen.Organization(t, db, database.Organization{})
	user := dbgen.User(t, db, database.User{})
	file := dbgen.File(t, db, database.File{})
	for i := 0; i < 5; i++ {
		dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
			CreatedAt: now.Add(-time.Minute * 5),
			UpdatedAt: now.Add(-time.Minute * time.Duration(i)),
			StartedAt: sql.NullTime{
				Time:  now.Add(-time.Minute * 5),
				Valid: true,
			},
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         file.ID,
			Type:           database.ProvisionerJobTypeWorkspaceBuild,
			Input:          []byte("{}"),
		})
	}

	detector := jobreaper.New(ctx, wrapDBAuthz(db, log), pubsub, log, tickCh).WithStatsChannel(statsCh)
	detector.Start()
	tickCh <- now

	stats := <-statsCh
	require.NoError(t, stats.Error)
	require.Empty(t, stats.TerminatedJobIDs)

	detector.Close()
	detector.Wait()
}

func TestDetectorHungWorkspaceBuild(t *testing.T) {
	t.Parallel()

	var (
		ctx        = testutil.Context(t, testutil.WaitLong)
		db, pubsub = dbtestutil.NewDB(t)
		log        = testutil.Logger(t)
		tickCh     = make(chan time.Time)
		statsCh    = make(chan jobreaper.Stats)
	)

	var (
		now          = time.Now()
		twentyMinAgo = now.Add(-time.Minute * 20)
		tenMinAgo    = now.Add(-time.Minute * 10)
		sixMinAgo    = now.Add(-time.Minute * 6)
		org          = dbgen.Organization(t, db, database.Organization{})
		user         = dbgen.User(t, db, database.User{})
		file         = dbgen.File(t, db, database.File{})
		template     = dbgen.Template(t, db, database.Template{
			OrganizationID: org.ID,
			CreatedBy:      user.ID,
		})
		templateVersion = dbgen.TemplateVersion(t, db, database.TemplateVersion{
			OrganizationID: org.ID,
			TemplateID: uuid.NullUUID{
				UUID:  template.ID,
				Valid: true,
			},
			CreatedBy: user.ID,
		})
		workspace = dbgen.Workspace(t, db, database.WorkspaceTable{
			OwnerID:        user.ID,
			OrganizationID: org.ID,
			TemplateID:     template.ID,
		})

		// Previous build.
		expectedWorkspaceBuildState = []byte(`{"dean":"cool","colin":"also cool"}`)
		previousWorkspaceBuildJob   = dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
			CreatedAt: twentyMinAgo,
			UpdatedAt: twentyMinAgo,
			StartedAt: sql.NullTime{
				Time:  twentyMinAgo,
				Valid: true,
			},
			CompletedAt: sql.NullTime{
				Time:  twentyMinAgo,
				Valid: true,
			},
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         file.ID,
			Type:           database.ProvisionerJobTypeWorkspaceBuild,
			Input:          []byte("{}"),
		})
		_ = dbgen.WorkspaceBuild(t, db, database.WorkspaceBuild{
			WorkspaceID:       workspace.ID,
			TemplateVersionID: templateVersion.ID,
			BuildNumber:       1,
			ProvisionerState:  expectedWorkspaceBuildState,
			JobID:             previousWorkspaceBuildJob.ID,
		})

		// Current build.
		currentWorkspaceBuildJob = dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
			CreatedAt: tenMinAgo,
			UpdatedAt: sixMinAgo,
			StartedAt: sql.NullTime{
				Time:  tenMinAgo,
				Valid: true,
			},
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         file.ID,
			Type:           database.ProvisionerJobTypeWorkspaceBuild,
			Input:          []byte("{}"),
		})
		currentWorkspaceBuild = dbgen.WorkspaceBuild(t, db, database.WorkspaceBuild{
			WorkspaceID:       workspace.ID,
			TemplateVersionID: templateVersion.ID,
			BuildNumber:       2,
			JobID:             currentWorkspaceBuildJob.ID,
			// No provisioner state.
		})
	)

	t.Log("previous job ID: ", previousWorkspaceBuildJob.ID)
	t.Log("current job ID: ", currentWorkspaceBuildJob.ID)

	detector := jobreaper.New(ctx, wrapDBAuthz(db, log), pubsub, log, tickCh).WithStatsChannel(statsCh)
	detector.Start()
	tickCh <- now

	stats := <-statsCh
	require.NoError(t, stats.Error)
	require.Len(t, stats.TerminatedJobIDs, 1)
	require.Equal(t, currentWorkspaceBuildJob.ID, stats.TerminatedJobIDs[0])

	// Check that the current provisioner job was updated.
	job, err := db.GetProvisionerJobByID(ctx, currentWorkspaceBuildJob.ID)
	require.NoError(t, err)
	require.WithinDuration(t, now, job.UpdatedAt, 30*time.Second)
	require.True(t, job.CompletedAt.Valid)
	require.WithinDuration(t, now, job.CompletedAt.Time, 30*time.Second)
	require.True(t, job.Error.Valid)
	require.Contains(t, job.Error.String, "Build has been detected as hung")
	require.False(t, job.ErrorCode.Valid)

	// Check that the provisioner state was copied.
	build, err := db.GetWorkspaceBuildByID(ctx, currentWorkspaceBuild.ID)
	require.NoError(t, err)
	require.Equal(t, expectedWorkspaceBuildState, build.ProvisionerState)

	detector.Close()
	detector.Wait()
}

func TestDetectorHungWorkspaceBuildNoOverrideState(t *testing.T) {
	t.Parallel()

	var (
		ctx        = testutil.Context(t, testutil.WaitLong)
		db, pubsub = dbtestutil.NewDB(t)
		log        = testutil.Logger(t)
		tickCh     = make(chan time.Time)
		statsCh    = make(chan jobreaper.Stats)
	)

	var (
		now          = time.Now()
		twentyMinAgo = now.Add(-time.Minute * 20)
		tenMinAgo    = now.Add(-time.Minute * 10)
		sixMinAgo    = now.Add(-time.Minute * 6)
		org          = dbgen.Organization(t, db, database.Organization{})
		user         = dbgen.User(t, db, database.User{})
		file         = dbgen.File(t, db, database.File{})
		template     = dbgen.Template(t, db, database.Template{
			OrganizationID: org.ID,
			CreatedBy:      user.ID,
		})
		templateVersion = dbgen.TemplateVersion(t, db, database.TemplateVersion{
			OrganizationID: org.ID,
			TemplateID: uuid.NullUUID{
				UUID:  template.ID,
				Valid: true,
			},
			CreatedBy: user.ID,
		})
		workspace = dbgen.Workspace(t, db, database.WorkspaceTable{
			OwnerID:        user.ID,
			OrganizationID: org.ID,
			TemplateID:     template.ID,
		})

		// Previous build.
		previousWorkspaceBuildJob = dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
			CreatedAt: twentyMinAgo,
			UpdatedAt: twentyMinAgo,
			StartedAt: sql.NullTime{
				Time:  twentyMinAgo,
				Valid: true,
			},
			CompletedAt: sql.NullTime{
				Time:  twentyMinAgo,
				Valid: true,
			},
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         file.ID,
			Type:           database.ProvisionerJobTypeWorkspaceBuild,
			Input:          []byte("{}"),
		})
		_ = dbgen.WorkspaceBuild(t, db, database.WorkspaceBuild{
			WorkspaceID:       workspace.ID,
			TemplateVersionID: templateVersion.ID,
			BuildNumber:       1,
			ProvisionerState:  []byte(`{"dean":"NOT cool","colin":"also NOT cool"}`),
			JobID:             previousWorkspaceBuildJob.ID,
		})

		// Current build.
		expectedWorkspaceBuildState = []byte(`{"dean":"cool","colin":"also cool"}`)
		currentWorkspaceBuildJob    = dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
			CreatedAt: tenMinAgo,
			UpdatedAt: sixMinAgo,
			StartedAt: sql.NullTime{
				Time:  tenMinAgo,
				Valid: true,
			},
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         file.ID,
			Type:           database.ProvisionerJobTypeWorkspaceBuild,
			Input:          []byte("{}"),
		})
		currentWorkspaceBuild = dbgen.WorkspaceBuild(t, db, database.WorkspaceBuild{
			WorkspaceID:       workspace.ID,
			TemplateVersionID: templateVersion.ID,
			BuildNumber:       2,
			JobID:             currentWorkspaceBuildJob.ID,
			// Should not be overridden.
			ProvisionerState: expectedWorkspaceBuildState,
		})
	)

	t.Log("previous job ID: ", previousWorkspaceBuildJob.ID)
	t.Log("current job ID: ", currentWorkspaceBuildJob.ID)

	detector := jobreaper.New(ctx, wrapDBAuthz(db, log), pubsub, log, tickCh).WithStatsChannel(statsCh)
	detector.Start()
	tickCh <- now

	stats := <-statsCh
	require.NoError(t, stats.Error)
	require.Len(t, stats.TerminatedJobIDs, 1)
	require.Equal(t, currentWorkspaceBuildJob.ID, stats.TerminatedJobIDs[0])

	// Check that the current provisioner job was updated.
	job, err := db.GetProvisionerJobByID(ctx, currentWorkspaceBuildJob.ID)
	require.NoError(t, err)
	require.WithinDuration(t, now, job.UpdatedAt, 30*time.Second)
	require.True(t, job.CompletedAt.Valid)
	require.WithinDuration(t, now, job.CompletedAt.Time, 30*time.Second)
	require.True(t, job.Error.Valid)
	require.Contains(t, job.Error.String, "Build has been detected as hung")
	require.False(t, job.ErrorCode.Valid)

	// Check that the provisioner state was NOT copied.
	build, err := db.GetWorkspaceBuildByID(ctx, currentWorkspaceBuild.ID)
	require.NoError(t, err)
	require.Equal(t, expectedWorkspaceBuildState, build.ProvisionerState)

	detector.Close()
	detector.Wait()
}

func TestDetectorHungWorkspaceBuildNoOverrideStateIfNoExistingBuild(t *testing.T) {
	t.Parallel()

	var (
		ctx        = testutil.Context(t, testutil.WaitLong)
		db, pubsub = dbtestutil.NewDB(t)
		log        = testutil.Logger(t)
		tickCh     = make(chan time.Time)
		statsCh    = make(chan jobreaper.Stats)
	)

	var (
		now       = time.Now()
		tenMinAgo = now.Add(-time.Minute * 10)
		sixMinAgo = now.Add(-time.Minute * 6)
		org       = dbgen.Organization(t, db, database.Organization{})
		user      = dbgen.User(t, db, database.User{})
		file      = dbgen.File(t, db, database.File{})
		template  = dbgen.Template(t, db, database.Template{
			OrganizationID: org.ID,
			CreatedBy:      user.ID,
		})
		templateVersion = dbgen.TemplateVersion(t, db, database.TemplateVersion{
			OrganizationID: org.ID,
			TemplateID: uuid.NullUUID{
				UUID:  template.ID,
				Valid: true,
			},
			CreatedBy: user.ID,
		})
		workspace = dbgen.Workspace(t, db, database.WorkspaceTable{
			OwnerID:        user.ID,
			OrganizationID: org.ID,
			TemplateID:     template.ID,
		})

		// First build.
		expectedWorkspaceBuildState = []byte(`{"dean":"cool","colin":"also cool"}`)
		currentWorkspaceBuildJob    = dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
			CreatedAt: tenMinAgo,
			UpdatedAt: sixMinAgo,
			StartedAt: sql.NullTime{
				Time:  tenMinAgo,
				Valid: true,
			},
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         file.ID,
			Type:           database.ProvisionerJobTypeWorkspaceBuild,
			Input:          []byte("{}"),
		})
		currentWorkspaceBuild = dbgen.WorkspaceBuild(t, db, database.WorkspaceBuild{
			WorkspaceID:       workspace.ID,
			TemplateVersionID: templateVersion.ID,
			BuildNumber:       1,
			JobID:             currentWorkspaceBuildJob.ID,
			// Should not be overridden.
			ProvisionerState: expectedWorkspaceBuildState,
		})
	)

	t.Log("current job ID: ", currentWorkspaceBuildJob.ID)

	detector := jobreaper.New(ctx, wrapDBAuthz(db, log), pubsub, log, tickCh).WithStatsChannel(statsCh)
	detector.Start()
	tickCh <- now

	stats := <-statsCh
	require.NoError(t, stats.Error)
	require.Len(t, stats.TerminatedJobIDs, 1)
	require.Equal(t, currentWorkspaceBuildJob.ID, stats.TerminatedJobIDs[0])

	// Check that the current provisioner job was updated.
	job, err := db.GetProvisionerJobByID(ctx, currentWorkspaceBuildJob.ID)
	require.NoError(t, err)
	require.WithinDuration(t, now, job.UpdatedAt, 30*time.Second)
	require.True(t, job.CompletedAt.Valid)
	require.WithinDuration(t, now, job.CompletedAt.Time, 30*time.Second)
	require.True(t, job.Error.Valid)
	require.Contains(t, job.Error.String, "Build has been detected as hung")
	require.False(t, job.ErrorCode.Valid)

	// Check that the provisioner state was NOT updated.
	build, err := db.GetWorkspaceBuildByID(ctx, currentWorkspaceBuild.ID)
	require.NoError(t, err)
	require.Equal(t, expectedWorkspaceBuildState, build.ProvisionerState)

	detector.Close()
	detector.Wait()
}

func TestDetectorPendingWorkspaceBuildNoOverrideStateIfNoExistingBuild(t *testing.T) {
	t.Parallel()

	var (
		ctx        = testutil.Context(t, testutil.WaitLong)
		db, pubsub = dbtestutil.NewDB(t)
		log        = testutil.Logger(t)
		tickCh     = make(chan time.Time)
		statsCh    = make(chan jobreaper.Stats)
	)

	var (
		now              = time.Now()
		thirtyFiveMinAgo = now.Add(-time.Minute * 35)
		org              = dbgen.Organization(t, db, database.Organization{})
		user             = dbgen.User(t, db, database.User{})
		file             = dbgen.File(t, db, database.File{})
		template         = dbgen.Template(t, db, database.Template{
			OrganizationID: org.ID,
			CreatedBy:      user.ID,
		})
		templateVersion = dbgen.TemplateVersion(t, db, database.TemplateVersion{
			OrganizationID: org.ID,
			TemplateID: uuid.NullUUID{
				UUID:  template.ID,
				Valid: true,
			},
			CreatedBy: user.ID,
		})
		workspace = dbgen.Workspace(t, db, database.WorkspaceTable{
			OwnerID:        user.ID,
			OrganizationID: org.ID,
			TemplateID:     template.ID,
		})

		// First build.
		expectedWorkspaceBuildState = []byte(`{"dean":"cool","colin":"also cool"}`)
		currentWorkspaceBuildJob    = dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
			CreatedAt: thirtyFiveMinAgo,
			UpdatedAt: thirtyFiveMinAgo,
			StartedAt: sql.NullTime{
				Time:  time.Time{},
				Valid: false,
			},
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         file.ID,
			Type:           database.ProvisionerJobTypeWorkspaceBuild,
			Input:          []byte("{}"),
		})
		currentWorkspaceBuild = dbgen.WorkspaceBuild(t, db, database.WorkspaceBuild{
			WorkspaceID:       workspace.ID,
			TemplateVersionID: templateVersion.ID,
			BuildNumber:       1,
			JobID:             currentWorkspaceBuildJob.ID,
			// Should not be overridden.
			ProvisionerState: expectedWorkspaceBuildState,
		})
	)

	t.Log("current job ID: ", currentWorkspaceBuildJob.ID)

	detector := jobreaper.New(ctx, wrapDBAuthz(db, log), pubsub, log, tickCh).WithStatsChannel(statsCh)
	detector.Start()
	tickCh <- now

	stats := <-statsCh
	require.NoError(t, stats.Error)
	require.Len(t, stats.TerminatedJobIDs, 1)
	require.Equal(t, currentWorkspaceBuildJob.ID, stats.TerminatedJobIDs[0])

	// Check that the current provisioner job was updated.
	job, err := db.GetProvisionerJobByID(ctx, currentWorkspaceBuildJob.ID)
	require.NoError(t, err)
	require.WithinDuration(t, now, job.UpdatedAt, 30*time.Second)
	require.True(t, job.CompletedAt.Valid)
	require.WithinDuration(t, now, job.CompletedAt.Time, 30*time.Second)
	require.True(t, job.StartedAt.Valid)
	require.WithinDuration(t, now, job.StartedAt.Time, 30*time.Second)
	require.True(t, job.Error.Valid)
	require.Contains(t, job.Error.String, "Build has been detected as pending")
	require.False(t, job.ErrorCode.Valid)

	// Check that the provisioner state was NOT updated.
	build, err := db.GetWorkspaceBuildByID(ctx, currentWorkspaceBuild.ID)
	require.NoError(t, err)
	require.Equal(t, expectedWorkspaceBuildState, build.ProvisionerState)

	detector.Close()
	detector.Wait()
}

func TestDetectorHungOtherJobTypes(t *testing.T) {
	t.Parallel()

	var (
		ctx        = testutil.Context(t, testutil.WaitLong)
		db, pubsub = dbtestutil.NewDB(t)
		log        = testutil.Logger(t)
		tickCh     = make(chan time.Time)
		statsCh    = make(chan jobreaper.Stats)
	)

	var (
		now       = time.Now()
		tenMinAgo = now.Add(-time.Minute * 10)
		sixMinAgo = now.Add(-time.Minute * 6)
		org       = dbgen.Organization(t, db, database.Organization{})
		user      = dbgen.User(t, db, database.User{})
		file      = dbgen.File(t, db, database.File{})

		// Template import job.
		templateImportJob = dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
			CreatedAt: tenMinAgo,
			UpdatedAt: sixMinAgo,
			StartedAt: sql.NullTime{
				Time:  tenMinAgo,
				Valid: true,
			},
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         file.ID,
			Type:           database.ProvisionerJobTypeTemplateVersionImport,
			Input:          []byte("{}"),
		})
		_ = dbgen.TemplateVersion(t, db, database.TemplateVersion{
			OrganizationID: org.ID,
			JobID:          templateImportJob.ID,
			CreatedBy:      user.ID,
		})
	)

	// Template dry-run job.
	dryRunVersion := dbgen.TemplateVersion(t, db, database.TemplateVersion{
		OrganizationID: org.ID,
		CreatedBy:      user.ID,
	})
	input, err := json.Marshal(provisionerdserver.TemplateVersionDryRunJob{
		TemplateVersionID: dryRunVersion.ID,
	})
	require.NoError(t, err)
	templateDryRunJob := dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
		CreatedAt: tenMinAgo,
		UpdatedAt: sixMinAgo,
		StartedAt: sql.NullTime{
			Time:  tenMinAgo,
			Valid: true,
		},
		OrganizationID: org.ID,
		InitiatorID:    user.ID,
		Provisioner:    database.ProvisionerTypeEcho,
		StorageMethod:  database.ProvisionerStorageMethodFile,
		FileID:         file.ID,
		Type:           database.ProvisionerJobTypeTemplateVersionDryRun,
		Input:          input,
	})

	t.Log("template import job ID: ", templateImportJob.ID)
	t.Log("template dry-run job ID: ", templateDryRunJob.ID)

	detector := jobreaper.New(ctx, wrapDBAuthz(db, log), pubsub, log, tickCh).WithStatsChannel(statsCh)
	detector.Start()
	tickCh <- now

	stats := <-statsCh
	require.NoError(t, stats.Error)
	require.Len(t, stats.TerminatedJobIDs, 2)
	require.Contains(t, stats.TerminatedJobIDs, templateImportJob.ID)
	require.Contains(t, stats.TerminatedJobIDs, templateDryRunJob.ID)

	// Check that the template import job was updated.
	job, err := db.GetProvisionerJobByID(ctx, templateImportJob.ID)
	require.NoError(t, err)
	require.WithinDuration(t, now, job.UpdatedAt, 30*time.Second)
	require.True(t, job.CompletedAt.Valid)
	require.WithinDuration(t, now, job.CompletedAt.Time, 30*time.Second)
	require.True(t, job.Error.Valid)
	require.Contains(t, job.Error.String, "Build has been detected as hung")
	require.False(t, job.ErrorCode.Valid)

	// Check that the template dry-run job was updated.
	job, err = db.GetProvisionerJobByID(ctx, templateDryRunJob.ID)
	require.NoError(t, err)
	require.WithinDuration(t, now, job.UpdatedAt, 30*time.Second)
	require.True(t, job.CompletedAt.Valid)
	require.WithinDuration(t, now, job.CompletedAt.Time, 30*time.Second)
	require.True(t, job.Error.Valid)
	require.Contains(t, job.Error.String, "Build has been detected as hung")
	require.False(t, job.ErrorCode.Valid)

	detector.Close()
	detector.Wait()
}

func TestDetectorPendingOtherJobTypes(t *testing.T) {
	t.Parallel()

	var (
		ctx        = testutil.Context(t, testutil.WaitLong)
		db, pubsub = dbtestutil.NewDB(t)
		log        = testutil.Logger(t)
		tickCh     = make(chan time.Time)
		statsCh    = make(chan jobreaper.Stats)
	)

	var (
		now              = time.Now()
		thirtyFiveMinAgo = now.Add(-time.Minute * 35)
		org              = dbgen.Organization(t, db, database.Organization{})
		user             = dbgen.User(t, db, database.User{})
		file             = dbgen.File(t, db, database.File{})

		// Template import job.
		templateImportJob = dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
			CreatedAt: thirtyFiveMinAgo,
			UpdatedAt: thirtyFiveMinAgo,
			StartedAt: sql.NullTime{
				Time:  time.Time{},
				Valid: false,
			},
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         file.ID,
			Type:           database.ProvisionerJobTypeTemplateVersionImport,
			Input:          []byte("{}"),
		})
		_ = dbgen.TemplateVersion(t, db, database.TemplateVersion{
			OrganizationID: org.ID,
			JobID:          templateImportJob.ID,
			CreatedBy:      user.ID,
		})
	)

	// Template dry-run job.
	dryRunVersion := dbgen.TemplateVersion(t, db, database.TemplateVersion{
		OrganizationID: org.ID,
		CreatedBy:      user.ID,
	})
	input, err := json.Marshal(provisionerdserver.TemplateVersionDryRunJob{
		TemplateVersionID: dryRunVersion.ID,
	})
	require.NoError(t, err)
	templateDryRunJob := dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
		CreatedAt: thirtyFiveMinAgo,
		UpdatedAt: thirtyFiveMinAgo,
		StartedAt: sql.NullTime{
			Time:  time.Time{},
			Valid: false,
		},
		OrganizationID: org.ID,
		InitiatorID:    user.ID,
		Provisioner:    database.ProvisionerTypeEcho,
		StorageMethod:  database.ProvisionerStorageMethodFile,
		FileID:         file.ID,
		Type:           database.ProvisionerJobTypeTemplateVersionDryRun,
		Input:          input,
	})

	t.Log("template import job ID: ", templateImportJob.ID)
	t.Log("template dry-run job ID: ", templateDryRunJob.ID)

	detector := jobreaper.New(ctx, wrapDBAuthz(db, log), pubsub, log, tickCh).WithStatsChannel(statsCh)
	detector.Start()
	tickCh <- now

	stats := <-statsCh
	require.NoError(t, stats.Error)
	require.Len(t, stats.TerminatedJobIDs, 2)
	require.Contains(t, stats.TerminatedJobIDs, templateImportJob.ID)
	require.Contains(t, stats.TerminatedJobIDs, templateDryRunJob.ID)

	// Check that the template import job was updated.
	job, err := db.GetProvisionerJobByID(ctx, templateImportJob.ID)
	require.NoError(t, err)
	require.WithinDuration(t, now, job.UpdatedAt, 30*time.Second)
	require.True(t, job.CompletedAt.Valid)
	require.WithinDuration(t, now, job.CompletedAt.Time, 30*time.Second)
	require.True(t, job.StartedAt.Valid)
	require.WithinDuration(t, now, job.StartedAt.Time, 30*time.Second)
	require.True(t, job.Error.Valid)
	require.Contains(t, job.Error.String, "Build has been detected as pending")
	require.False(t, job.ErrorCode.Valid)

	// Check that the template dry-run job was updated.
	job, err = db.GetProvisionerJobByID(ctx, templateDryRunJob.ID)
	require.NoError(t, err)
	require.WithinDuration(t, now, job.UpdatedAt, 30*time.Second)
	require.True(t, job.CompletedAt.Valid)
	require.WithinDuration(t, now, job.CompletedAt.Time, 30*time.Second)
	require.True(t, job.StartedAt.Valid)
	require.WithinDuration(t, now, job.StartedAt.Time, 30*time.Second)
	require.True(t, job.Error.Valid)
	require.Contains(t, job.Error.String, "Build has been detected as pending")
	require.False(t, job.ErrorCode.Valid)

	detector.Close()
	detector.Wait()
}

func TestDetectorHungCanceledJob(t *testing.T) {
	t.Parallel()

	var (
		ctx        = testutil.Context(t, testutil.WaitLong)
		db, pubsub = dbtestutil.NewDB(t)
		log        = testutil.Logger(t)
		tickCh     = make(chan time.Time)
		statsCh    = make(chan jobreaper.Stats)
	)

	var (
		now       = time.Now()
		tenMinAgo = now.Add(-time.Minute * 10)
		sixMinAgo = now.Add(-time.Minute * 6)
		org       = dbgen.Organization(t, db, database.Organization{})
		user      = dbgen.User(t, db, database.User{})
		file      = dbgen.File(t, db, database.File{})

		// Template import job.
		templateImportJob = dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
			CreatedAt: tenMinAgo,
			CanceledAt: sql.NullTime{
				Time:  tenMinAgo,
				Valid: true,
			},
			UpdatedAt: sixMinAgo,
			StartedAt: sql.NullTime{
				Time:  tenMinAgo,
				Valid: true,
			},
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         file.ID,
			Type:           database.ProvisionerJobTypeTemplateVersionImport,
			Input:          []byte("{}"),
		})
		_ = dbgen.TemplateVersion(t, db, database.TemplateVersion{
			OrganizationID: org.ID,
			JobID:          templateImportJob.ID,
			CreatedBy:      user.ID,
		})
	)

	t.Log("template import job ID: ", templateImportJob.ID)

	detector := jobreaper.New(ctx, wrapDBAuthz(db, log), pubsub, log, tickCh).WithStatsChannel(statsCh)
	detector.Start()
	tickCh <- now

	stats := <-statsCh
	require.NoError(t, stats.Error)
	require.Len(t, stats.TerminatedJobIDs, 1)
	require.Contains(t, stats.TerminatedJobIDs, templateImportJob.ID)

	// Check that the job was updated.
	job, err := db.GetProvisionerJobByID(ctx, templateImportJob.ID)
	require.NoError(t, err)
	require.WithinDuration(t, now, job.UpdatedAt, 30*time.Second)
	require.True(t, job.CompletedAt.Valid)
	require.WithinDuration(t, now, job.CompletedAt.Time, 30*time.Second)
	require.True(t, job.Error.Valid)
	require.Contains(t, job.Error.String, "Build has been detected as hung")
	require.False(t, job.ErrorCode.Valid)

	detector.Close()
	detector.Wait()
}

func TestDetectorPushesLogs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		preLogCount int
		preLogStage string
		expectStage string
	}{
		{
			name:        "WithExistingLogs",
			preLogCount: 10,
			preLogStage: "Stage Name",
			expectStage: "Stage Name",
		},
		{
			name:        "WithExistingLogsNoStage",
			preLogCount: 10,
			preLogStage: "",
			expectStage: "Unknown",
		},
		{
			name:        "WithoutExistingLogs",
			preLogCount: 0,
			expectStage: "Unknown",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			var (
				ctx        = testutil.Context(t, testutil.WaitLong)
				db, pubsub = dbtestutil.NewDB(t)
				log        = testutil.Logger(t)
				tickCh     = make(chan time.Time)
				statsCh    = make(chan jobreaper.Stats)
			)

			var (
				now       = time.Now()
				tenMinAgo = now.Add(-time.Minute * 10)
				sixMinAgo = now.Add(-time.Minute * 6)
				org       = dbgen.Organization(t, db, database.Organization{})
				user      = dbgen.User(t, db, database.User{})
				file      = dbgen.File(t, db, database.File{})

				// Template import job.
				templateImportJob = dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
					CreatedAt: tenMinAgo,
					UpdatedAt: sixMinAgo,
					StartedAt: sql.NullTime{
						Time:  tenMinAgo,
						Valid: true,
					},
					OrganizationID: org.ID,
					InitiatorID:    user.ID,
					Provisioner:    database.ProvisionerTypeEcho,
					StorageMethod:  database.ProvisionerStorageMethodFile,
					FileID:         file.ID,
					Type:           database.ProvisionerJobTypeTemplateVersionImport,
					Input:          []byte("{}"),
				})
				_ = dbgen.TemplateVersion(t, db, database.TemplateVersion{
					OrganizationID: org.ID,
					JobID:          templateImportJob.ID,
					CreatedBy:      user.ID,
				})
			)

			t.Log("template import job ID: ", templateImportJob.ID)

			// Insert some logs at the start of the job.
			if c.preLogCount > 0 {
				insertParams := database.InsertProvisionerJobLogsParams{
					JobID: templateImportJob.ID,
				}
				for i := 0; i < c.preLogCount; i++ {
					insertParams.CreatedAt = append(insertParams.CreatedAt, tenMinAgo.Add(time.Millisecond*time.Duration(i)))
					insertParams.Level = append(insertParams.Level, database.LogLevelInfo)
					insertParams.Stage = append(insertParams.Stage, c.preLogStage)
					insertParams.Source = append(insertParams.Source, database.LogSourceProvisioner)
					insertParams.Output = append(insertParams.Output, fmt.Sprintf("Output %d", i))
				}
				logs, err := db.InsertProvisionerJobLogs(ctx, insertParams)
				require.NoError(t, err)
				require.Len(t, logs, 10)
			}

			detector := jobreaper.New(ctx, wrapDBAuthz(db, log), pubsub, log, tickCh).WithStatsChannel(statsCh)
			detector.Start()

			// Create pubsub subscription to listen for new log events.
			pubsubCalled := make(chan int64, 1)
			pubsubCancel, err := pubsub.Subscribe(provisionersdk.ProvisionerJobLogsNotifyChannel(templateImportJob.ID), func(ctx context.Context, message []byte) {
				defer close(pubsubCalled)
				var event provisionersdk.ProvisionerJobLogsNotifyMessage
				err := json.Unmarshal(message, &event)
				if !assert.NoError(t, err) {
					return
				}

				assert.True(t, event.EndOfLogs)
				pubsubCalled <- event.CreatedAfter
			})
			require.NoError(t, err)
			defer pubsubCancel()

			tickCh <- now

			stats := <-statsCh
			require.NoError(t, stats.Error)
			require.Len(t, stats.TerminatedJobIDs, 1)
			require.Contains(t, stats.TerminatedJobIDs, templateImportJob.ID)

			after := <-pubsubCalled

			// Get the jobs after the given time and check that they are what we
			// expect.
			logs, err := db.GetProvisionerLogsAfterID(ctx, database.GetProvisionerLogsAfterIDParams{
				JobID:        templateImportJob.ID,
				CreatedAfter: after,
			})
			require.NoError(t, err)
			threshold := jobreaper.HungJobDuration
			jobType := jobreaper.Hung
			if templateImportJob.JobStatus == database.ProvisionerJobStatusPending {
				threshold = jobreaper.PendingJobDuration
				jobType = jobreaper.Pending
			}
			expectedLogs := jobreaper.JobLogMessages(jobType, threshold)
			require.Len(t, logs, len(expectedLogs))
			for i, log := range logs {
				assert.Equal(t, database.LogLevelError, log.Level)
				assert.Equal(t, c.expectStage, log.Stage)
				assert.Equal(t, database.LogSourceProvisionerDaemon, log.Source)
				assert.Equal(t, expectedLogs[i], log.Output)
			}

			// Double check the full log count.
			logs, err = db.GetProvisionerLogsAfterID(ctx, database.GetProvisionerLogsAfterIDParams{
				JobID:        templateImportJob.ID,
				CreatedAfter: 0,
			})
			require.NoError(t, err)
			require.Len(t, logs, c.preLogCount+len(expectedLogs))

			detector.Close()
			detector.Wait()
		})
	}
}

func TestDetectorMaxJobsPerRun(t *testing.T) {
	t.Parallel()

	var (
		ctx        = testutil.Context(t, testutil.WaitLong)
		db, pubsub = dbtestutil.NewDB(t)
		log        = testutil.Logger(t)
		tickCh     = make(chan time.Time)
		statsCh    = make(chan jobreaper.Stats)
		org        = dbgen.Organization(t, db, database.Organization{})
		user       = dbgen.User(t, db, database.User{})
		file       = dbgen.File(t, db, database.File{})
	)

	// Create MaxJobsPerRun + 1 hung jobs.
	now := time.Now()
	for i := 0; i < jobreaper.MaxJobsPerRun+1; i++ {
		pj := dbgen.ProvisionerJob(t, db, pubsub, database.ProvisionerJob{
			CreatedAt: now.Add(-time.Hour),
			UpdatedAt: now.Add(-time.Hour),
			StartedAt: sql.NullTime{
				Time:  now.Add(-time.Hour),
				Valid: true,
			},
			OrganizationID: org.ID,
			InitiatorID:    user.ID,
			Provisioner:    database.ProvisionerTypeEcho,
			StorageMethod:  database.ProvisionerStorageMethodFile,
			FileID:         file.ID,
			Type:           database.ProvisionerJobTypeTemplateVersionImport,
			Input:          []byte("{}"),
		})
		_ = dbgen.TemplateVersion(t, db, database.TemplateVersion{
			OrganizationID: org.ID,
			JobID:          pj.ID,
			CreatedBy:      user.ID,
		})
	}

	detector := jobreaper.New(ctx, wrapDBAuthz(db, log), pubsub, log, tickCh).WithStatsChannel(statsCh)
	detector.Start()
	tickCh <- now

	// Make sure that only MaxJobsPerRun jobs are terminated.
	stats := <-statsCh
	require.NoError(t, stats.Error)
	require.Len(t, stats.TerminatedJobIDs, jobreaper.MaxJobsPerRun)

	// Run the detector again and make sure that only the remaining job is
	// terminated.
	tickCh <- now
	stats = <-statsCh
	require.NoError(t, stats.Error)
	require.Len(t, stats.TerminatedJobIDs, 1)

	detector.Close()
	detector.Wait()
}

// wrapDBAuthz adds our Authorization/RBAC around the given database store, to
// ensure the reaper has the right permissions to do its work.
func wrapDBAuthz(db database.Store, logger slog.Logger) database.Store {
	return dbauthz.New(
		db,
		rbac.NewStrictCachingAuthorizer(prometheus.NewRegistry()),
		logger,
		coderdtest.AccessControlStorePointer(),
	)
}
