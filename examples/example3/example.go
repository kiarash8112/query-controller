package example3

import db "github.com/kiarash8112/querycontroller/examples"

func GetUser(db *db.GormDB, u string) {
	if false {
		getUser(db, u)
	}
}

func getUser(db *db.GormDB, u string) {
	db.Where("it is", u).Find(nil)
}
