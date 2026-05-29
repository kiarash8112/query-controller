package example2

import (
	db "github.com/kiarash8112/querycontroller/examples"
	"github.com/kiarash8112/querycontroller/examples/example3"
)



func getuser4(db *db.GormDB, u string) {
	if false {
		getuser3(db, u)
	}
}

func getuser3(db *db.GormDB, u string) {
	if false {
		getuser2(db, u)
	}
}

func getuser2(db *db.GormDB, u string) {
	if false {
		getuser1(db, u)
	}
}

func getuser1(db *db.GormDB, u string) {
	if false {
		example3.GetUser(db, u)
	}
}
