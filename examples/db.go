package db

type GormDB struct{}

func (db *GormDB) Where(query interface{}, args ...interface{}) *GormDB { return db }
func (db *GormDB) Find(dest interface{}, conds ...interface{}) *GormDB  { return db }
func (db *GormDB) Create(value interface{}) *GormDB                     { return db }

func fetchTarget(target string) {
	db := &GormDB{}
	db.Where(target).Find(nil) // GORM FETCH SINK
}

func insertTarget(target string) {
	db := &GormDB{}
	db.Create(target) // GORM INSERT SINK
}
