// rasks-linkedin-helper - reads one person's LinkedIn inbox on their own Mac.
// Copyright (C) 2026 Rasks
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"encoding/json"
	"errors"

	"github.com/keybase/go-keychain"
)

// The session lives in the login Keychain, one generic password, never in a file.
// The item is the helper's own: macOS lets the binary that made it read it with no
// prompt, and asks before anything else does.
const (
	keychainService = "Rasks LinkedIn"
	keychainAccount = "session"
)

// errNoSession: nobody signed in on this Mac yet, or the person turned LinkedIn off.
var errNoSession = errors.New("no LinkedIn session in the Keychain: sign in again")

type keychainStore struct {
	service string
	account string
}

func (k keychainStore) query() keychain.Item {
	q := keychain.NewItem()
	q.SetSecClass(keychain.SecClassGenericPassword)
	q.SetService(k.service)
	q.SetAccount(k.account)
	return q
}

func (k keychainStore) Load() (Session, error) {
	var s Session
	q := k.query()
	q.SetMatchLimit(keychain.MatchLimitOne)
	q.SetReturnData(true)
	found, err := keychain.QueryItem(q)
	if err != nil {
		return s, err
	}
	if len(found) == 0 {
		return s, errNoSession
	}
	if err := json.Unmarshal(found[0].Data, &s); err != nil {
		return s, err
	}
	return s, nil
}

// Save writes the session, replacing the one there: LinkedIn rotates its cookies,
// and the next start needs the newest ones.
func (k keychainStore) Save(s Session) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	update := keychain.NewItem()
	update.SetData(data)
	err = keychain.UpdateItem(k.query(), update)
	if errors.Is(err, keychain.ErrorItemNotFound) {
		item := k.query()
		item.SetLabel("Rasks LinkedIn sign-in")
		item.SetDescription("The LinkedIn session Rasks reads your inbox with")
		item.SetAccessible(keychain.AccessibleAfterFirstUnlockThisDeviceOnly)
		item.SetSynchronizable(keychain.SynchronizableNo)
		item.SetData(data)
		return keychain.AddItem(item)
	}
	return err
}

func (k keychainStore) Forget() error {
	err := keychain.DeleteItem(k.query())
	if errors.Is(err, keychain.ErrorItemNotFound) {
		return nil
	}
	return err
}
