package multipackage

import "github.com/kiarash8112/querycontrolleranalyzer/code_examples/multi-package/product"

func GetPriceList(id int) {
	product.FetchProduct(id)
}
