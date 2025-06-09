package repository

type Repo struct {
	//
}

type IRepo interface {
	Create
	Get
	Udpate
	Del
}
