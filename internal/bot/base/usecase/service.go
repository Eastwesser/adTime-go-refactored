package usecase

type IBot interface {
	CreateUnit(b *Bot) (a string, err error)
	GetUnit(b *Bot) (a string, err error)
	UpdateUnit(b *Bot) (a string, err error)
	DeleteUnit(b *Bot) (a string, err error)
}

func NewBot() IBot {

}

func (b *Bot) CreateUnit() (a string, err error) {
	return
}

func (b *Bot) GetUnit() (rty string, err error) {
	return
}

func (b *Bot) UpdateUnit() (rty string, err error) {
	return
}

func (b *Bot) DeleteUnit() (a string, err error) {
	return
}
