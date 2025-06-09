package usecase

type Usecase struct {
	typorgaphy Typography
}

type ITypography interface {
	CreateSticker()
	ReadSticker()
	UpdateSticker()
	DeleteSticker()
}
