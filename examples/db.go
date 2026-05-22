package main

func fetchTarget(target string) {
	db := &GormDB{}
	db.Where(target).Find(nil) // GORM FETCH SINK
}

func insertTarget(target string) {
	db := &GormDB{}
	db.Create(target) // GORM INSERT SINK
}
