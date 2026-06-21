package recursion

// Minimal stand-in for hs.ir/r2/basic/job job_log.go: thin Tx/GormTx public
// wrappers funnel into logJob → doLogJob → logJob_checkAndInsert, which hits
// DB sinks with jobID / line / line_n2. Many wrappers over the same internal
// chain stress propagateSinkToPackageCallers the same way the real job package does.

type GormDB struct{}

func (db *GormDB) Exec(query interface{}, args ...interface{}) error { return nil }

type Ctx struct{}
type Tx struct{}

func (tx *Tx) Exec(query interface{}, args ...interface{}) error { return nil }

const (
	directDBIT = 1
	gormDBIT   = 2
)

type dbiParams struct {
	dbit   int
	tx     *Tx
	gormTx *GormDB
}

type LogType int

const (
	Info LogType = iota + 1
	Warning
	StepSucceed
)

func logJob(ctx *Ctx, dbip dbiParams, mustVerifyDBI bool, jobID int64,
	logType LogType, line string, line_n2 string) error {
	return doLogJob(ctx, dbip, mustVerifyDBI, jobID, logType, line, line_n2)
}

func doLogJob(ctx *Ctx, dbip dbiParams, mustVerifyDBI bool, jobID int64,
	logType LogType, line string, line_n2 string) error {
	if mustVerifyDBI {
		// second sink site: same params traced again (like middle calling sinkDB twice)
		if err := logJob_checkAndInsert(ctx, dbip, jobID, logType, line, line_n2); err != nil {
			return err
		}
	}
	return logJob_checkAndInsert(ctx, dbip, jobID, logType, line, line_n2)
}

func logJob_checkAndInsert(ctx *Ctx, dbip dbiParams, jobID int64,
	logType LogType, line string, line_n2 string) error {
	switch dbip.dbit {
	case directDBIT:
		return dbip.tx.Exec(`
			insert into bas_job_log (job_ref, log_type, line, line_n2)
			values ($1, $2, $3, $4)`, jobID, logType, line, line_n2)
	case gormDBIT:
		return dbip.gormTx.Exec(`
			insert into bas_job_log (job_ref, log_type, line, line_n2)
			values (?, ?, ?, ?)`, jobID, logType, line, line_n2)
	}
	return nil
}

func InfoTx(ctx *Ctx, tx *Tx, jobID int64, line string, line_n2 string) error {
	return logJob(ctx, dbiParams{dbit: directDBIT, tx: tx}, true, jobID, Info, line, line_n2)
}

func InfoGormTx(ctx *Ctx, gormTx *GormDB, jobID int64, line string, line_n2 string) error {
	return logJob(ctx, dbiParams{dbit: gormDBIT, gormTx: gormTx}, true, jobID, Info, line, line_n2)
}

func WarningTx(ctx *Ctx, tx *Tx, jobID int64, line string, line_n2 string) error {
	return logJob(ctx, dbiParams{dbit: directDBIT, tx: tx}, true, jobID, Warning, line, line_n2)
}

func WarningGormTx(ctx *Ctx, gormTx *GormDB, jobID int64, line string, line_n2 string) error {
	return logJob(ctx, dbiParams{dbit: gormDBIT, gormTx: gormTx}, true, jobID, Warning, line, line_n2)
}

func StepSucceedTx(ctx *Ctx, tx *Tx, jobID int64, line string, line_n2 string) error {
	return logJob(ctx, dbiParams{dbit: directDBIT, tx: tx}, true, jobID, StepSucceed, line, line_n2)
}

func StepSucceedGormTx(ctx *Ctx, gormTx *GormDB, jobID int64, line string, line_n2 string) error {
	return logJob(ctx, dbiParams{dbit: gormDBIT, gormTx: gormTx}, true, jobID, StepSucceed, line, line_n2)
}
