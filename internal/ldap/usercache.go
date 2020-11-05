package ldap

import (
	"crypto/md5"
	"crypto/tls"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-ldap/ldap/v3"
	log "github.com/sirupsen/logrus"
)

var Fields = []string{"givenName", "sn", "mail", "department", "memberOf", "sAMAccountName", "telephoneNumber",
	"mobile", "displayName", "cn", "title", "company", "manager", "streetAddress", "employeeID", "memberOf", "l",
	"st", "postalCode", "co", "facsimileTelephoneNumber", "pager", "thumbnailPhoto", "otherMobile",
	"extensionAttribute2", "distinguishedName", "userAccountControl"}

var ModifiableFields = []string{"department", "telephoneNumber", "mobile", "displayName", "title", "company",
	"manager", "streetAddress", "employeeID", "l", "st", "postalCode", "co", "thumbnailPhoto"}

// --------------------------------------------------------------------------------------------------------------------
// Cache Data Store
// --------------------------------------------------------------------------------------------------------------------

type UserCacheHolder interface {
	Clear()
	SetAllUsers(users []RawLdapData)
	SetUser(data RawLdapData)
	GetUser(dn string) *RawLdapData
	GetUsers() []*RawLdapData
}

type RawLdapData struct {
	DN            string
	Attributes    map[string]string
	RawAttributes map[string][][]byte
}

// --------------------------------------------------------------------------------------------------------------------
// Sample Cache Data store
// --------------------------------------------------------------------------------------------------------------------

type UserCacheHolderEntry struct {
	RawLdapData
	Username  string
	Mail      string
	Firstname string
	Lastname  string
	Groups    []string
}

func (e *UserCacheHolderEntry) CalcFieldsFromAttributes() {
	e.Username = strings.ToLower(e.Attributes["sAMAccountName"])
	e.Mail = e.Attributes["mail"]
	e.Firstname = e.Attributes["givenName"]
	e.Lastname = e.Attributes["sn"]
	e.Groups = make([]string, len(e.RawAttributes["memberOf"]))
	for i, group := range e.RawAttributes["memberOf"] {
		e.Groups[i] = string(group)
	}
}

func (e *UserCacheHolderEntry) GetUID() string {
	return fmt.Sprintf("u%x", md5.Sum([]byte(e.Attributes["distinguishedName"])))
}

type SynchronizedUserCacheHolder struct {
	users map[string]*UserCacheHolderEntry
	mux   sync.RWMutex
}

func (h *SynchronizedUserCacheHolder) Init() {
	h.users = make(map[string]*UserCacheHolderEntry)
}

func (h *SynchronizedUserCacheHolder) Clear() {
	h.mux.Lock()
	defer h.mux.Unlock()

	h.users = make(map[string]*UserCacheHolderEntry)
}

func (h *SynchronizedUserCacheHolder) SetAllUsers(users []RawLdapData) {
	h.mux.Lock()
	defer h.mux.Unlock()

	h.users = make(map[string]*UserCacheHolderEntry)

	for i := range users {
		h.users[users[i].DN] = &UserCacheHolderEntry{RawLdapData: users[i]}
		h.users[users[i].DN].CalcFieldsFromAttributes()
	}
}

func (h *SynchronizedUserCacheHolder) SetUser(user RawLdapData) {
	h.mux.Lock()
	defer h.mux.Unlock()

	h.users[user.DN] = &UserCacheHolderEntry{RawLdapData: user}
	h.users[user.DN].CalcFieldsFromAttributes()
}

func (h *SynchronizedUserCacheHolder) GetUser(dn string) *RawLdapData {
	h.mux.RLock()
	defer h.mux.RUnlock()

	return &h.users[dn].RawLdapData
}

func (h *SynchronizedUserCacheHolder) GetUserData(dn string) *UserCacheHolderEntry {
	h.mux.RLock()
	defer h.mux.RUnlock()

	return h.users[dn]
}

func (h *SynchronizedUserCacheHolder) GetUsers() []*RawLdapData {
	h.mux.RLock()
	defer h.mux.RUnlock()

	users := make([]*RawLdapData, 0, len(h.users))
	for _, user := range h.users {
		users = append(users, &user.RawLdapData)
	}

	return users
}

func (h *SynchronizedUserCacheHolder) GetSortedUsers(sortKey string, sortDirection string) []*UserCacheHolderEntry {
	h.mux.RLock()
	defer h.mux.RUnlock()

	sortedUsers := make([]*UserCacheHolderEntry, 0, len(h.users))

	for _, user := range h.users {
		sortedUsers = append(sortedUsers, user)
	}

	sort.Slice(sortedUsers, func(i, j int) bool {
		if sortDirection == "asc" {
			return sortedUsers[i].Attributes[sortKey] < sortedUsers[j].Attributes[sortKey]
		} else {
			return sortedUsers[i].Attributes[sortKey] > sortedUsers[j].Attributes[sortKey]
		}

	})

	return sortedUsers

}

func (h *SynchronizedUserCacheHolder) GetFilteredUsers(sortKey string, sortDirection string, search, searchDepartment string) []*UserCacheHolderEntry {
	sortedUsers := h.GetSortedUsers(sortKey, sortDirection)
	if search == "" && searchDepartment == "" {
		return sortedUsers // skip filtering
	}

	filteredUsers := make([]*UserCacheHolderEntry, 0, len(sortedUsers))
	for _, user := range sortedUsers {
		if searchDepartment != "" && user.Attributes["department"] != searchDepartment {
			continue
		}
		if strings.Contains(user.Attributes["sn"], search) ||
			strings.Contains(user.Attributes["givenName"], search) ||
			strings.Contains(user.Mail, search) ||
			strings.Contains(user.Attributes["department"], search) ||
			strings.Contains(user.Attributes["telephoneNumber"], search) ||
			strings.Contains(user.Attributes["mobile"], search) {
			filteredUsers = append(filteredUsers, user)
		}
	}

	return filteredUsers
}

func (h *SynchronizedUserCacheHolder) IsInGroup(username, gid string) bool {
	userDN := h.GetUserDN(username)
	if userDN == "" {
		return false // user not found -> not in group
	}

	user := h.GetUserData(userDN)
	if user == nil {
		return false
	}

	for _, group := range user.Groups {
		if group == gid {
			return true
		}
	}

	return false
}

func (h *SynchronizedUserCacheHolder) UserExists(username string) bool {
	userDN := h.GetUserDN(username)
	if userDN == "" {
		return false // user not found
	}

	return true
}

func (h *SynchronizedUserCacheHolder) GetUserDN(username string) string {
	userDN := ""
	for dn, user := range h.users {
		accName := strings.ToLower(user.Attributes["sAMAccountName"])
		if accName == username {
			userDN = dn
			break
		}
	}

	return userDN
}

func (h *SynchronizedUserCacheHolder) GetUserDNByMail(mail string) string {
	userDN := ""
	for dn, user := range h.users {
		accMail := strings.ToLower(user.Attributes["mail"])
		if accMail == mail {
			userDN = dn
			break
		}
	}

	return userDN
}

func (h *SynchronizedUserCacheHolder) GetTeamLeaders() []*UserCacheHolderEntry {

	sortedUsers := h.GetSortedUsers("sn", "asc")
	teamLeaders := make([]*UserCacheHolderEntry, 0, len(sortedUsers))
	for _, user := range sortedUsers {
		if user.Attributes["extensionAttribute2"] != "Teamleiter" {
			continue
		}

		teamLeaders = append(teamLeaders, user)
	}

	return teamLeaders
}

func (h *SynchronizedUserCacheHolder) GetDepartments() []string {
	h.mux.RLock()
	defer h.mux.RUnlock()

	departmentSet := make(map[string]struct{})
	for _, user := range h.users {
		if user.Attributes["department"] == "" {
			continue
		}
		departmentSet[user.Attributes["department"]] = struct{}{}
	}

	departments := make([]string, len(departmentSet))
	i := 0
	for department := range departmentSet {
		departments[i] = department
		i++
	}

	sort.Strings(departments)

	return departments
}

// --------------------------------------------------------------------------------------------------------------------
// Cache Handler, LDAP interaction
// --------------------------------------------------------------------------------------------------------------------

type UserCache struct {
	Cfg       *Config
	LastError error
	UpdatedAt time.Time
	userData  UserCacheHolder
}

func NewUserCache(config Config, store UserCacheHolder) *UserCache {
	uc := &UserCache{
		Cfg:       &config,
		UpdatedAt: time.Now(),
		userData:  store,
	}

	log.Infof("Filling user cache...")
	err := uc.Update(true)
	log.Infof("User cache filled!")
	uc.LastError = err

	return uc
}

func (u UserCache) open() (*ldap.Conn, error) {
	conn, err := ldap.DialURL(u.Cfg.URL)
	if err != nil {
		return nil, err
	}

	if u.Cfg.StartTLS {
		// Reconnect with TLS
		err = conn.StartTLS(&tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return nil, err
		}
	}

	err = conn.Bind(u.Cfg.BindUser, u.Cfg.BindPass)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

func (u UserCache) close(conn *ldap.Conn) {
	if conn != nil {
		conn.Close()
	}
}

// Update updates the user cache in background, minimal locking will happen
func (u *UserCache) Update(filter bool) error {
	log.Debugf("Updating ldap cache...")
	client, err := u.open()
	if err != nil {
		u.LastError = err
		return err
	}
	defer u.close(client)

	// Search for the given username
	searchRequest := ldap.NewSearchRequest(
		u.Cfg.BaseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(objectClass=organizationalPerson)",
		Fields,
		nil,
	)

	sr, err := client.Search(searchRequest)
	if err != nil {
		u.LastError = err
		return err
	}

	tmpData := make([]RawLdapData, 0, len(sr.Entries))

	for _, entry := range sr.Entries {
		if filter {
			usernameAttr := strings.ToLower(entry.GetAttributeValue("sAMAccountName"))
			firstNameAttr := entry.GetAttributeValue("givenName")
			lastNameAttr := entry.GetAttributeValue("sn")
			mailAttr := entry.GetAttributeValue("mail")
			userAccountControl := entry.GetAttributeValue("userAccountControl")
			employeeID := entry.GetAttributeValue("employeeID")
			dn := entry.GetAttributeValue("distinguishedName")

			if usernameAttr == "" || firstNameAttr == "" || lastNameAttr == "" || mailAttr == "" || employeeID == "" {
				continue // prefilter...
			}

			if userAccountControl == "" || userAccountControl == "514" {
				continue // 514 means account is disabled
			}

			if entry.DN != dn {
				log.Errorf("LDAP inconsistent: '%s' != '%s'", entry.DN, dn)
				continue
			}
		}

		tmp := RawLdapData{
			DN:            entry.DN,
			Attributes:    make(map[string]string, len(Fields)),
			RawAttributes: make(map[string][][]byte, len(Fields)),
		}

		for _, field := range Fields {
			tmp.Attributes[field] = entry.GetAttributeValue(field)
			tmp.RawAttributes[field] = entry.GetRawAttributeValues(field)
		}

		tmpData = append(tmpData, tmp)
	}

	// Copy to userdata
	u.userData.SetAllUsers(tmpData)
	u.UpdatedAt = time.Now()
	u.LastError = nil

	log.Debug("Ldap cache updated...")

	return nil
}

func (u *UserCache) ModifyUserData(dn string, newData RawLdapData, fields []string) error {
	if fields == nil {
		fields = ModifiableFields // default
	}

	existingUserData := u.userData.GetUser(dn)
	if existingUserData == nil {
		return fmt.Errorf("user with dn %s not found", dn)
	}

	modify := ldap.NewModifyRequest(dn, nil)

	for _, ldapAttribute := range fields {
		if existingUserData.Attributes[ldapAttribute] == newData.Attributes[ldapAttribute] {
			continue // do not update unchanged fields
		}

		if len(existingUserData.RawAttributes[ldapAttribute]) == 0 && newData.Attributes[ldapAttribute] != "" {
			modify.Add(ldapAttribute, []string{newData.Attributes[ldapAttribute]})
			newData.RawAttributes[ldapAttribute] = [][]byte{
				[]byte(newData.Attributes[ldapAttribute]),
			}
		}
		if len(existingUserData.RawAttributes[ldapAttribute]) != 0 && newData.Attributes[ldapAttribute] != "" {
			modify.Replace(ldapAttribute, []string{newData.Attributes[ldapAttribute]})
			newData.RawAttributes[ldapAttribute][0] = []byte(newData.Attributes[ldapAttribute])
		}
		if len(existingUserData.RawAttributes[ldapAttribute]) != 0 && newData.Attributes[ldapAttribute] == "" {
			modify.Delete(ldapAttribute, []string{})
			newData.RawAttributes[ldapAttribute] = [][]byte{} // clear list
		}
	}

	if len(modify.Changes) == 0 {
		return nil
	}

	client, err := u.open()
	if err != nil {
		u.LastError = err
		return err
	}
	defer u.close(client)

	err = client.Modify(modify)
	if err != nil {
		return err
	}

	// Once written to ldap, update the local cache
	u.userData.SetUser(newData)

	return nil
}
