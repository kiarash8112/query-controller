package example2

import (
	"fmt"

	db "github.com/kiarash8112/querycontroller/examples"
)

func simle(db *db.GormDB, u string) {
	getuser2(db, u)
}

func DoSome(){
	fmt.Println("did some ")
}