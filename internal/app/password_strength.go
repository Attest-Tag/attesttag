package app

import (
	"strings"
	"unicode"
)

// What makes a password unacceptable, past being too short.
//
// Length is the floor, not the policy. "1234567890" clears ten characters and is on the first
// page of every word list there is; so is "qwertyuiop", and so is "Password123". The argument
// against composition rules — that demanding a capital, a digit and a symbol produces
// "Password1!" and little else — is the same argument for judging the guess instead of its
// shape, which is what NIST asks for: compare against what people actually pick, and otherwise
// leave them alone.
//
// Three things have to be caught at ten characters or more:
//
//   - a word with decoration on it: password123, p@ssw0rd!, letmein2024
//   - a slide along the keyboard: 1234567890, qwertyuiop, 0987654321
//   - the account itself: the address, the person's name, their organisation's
//
// None of this is entropy estimation, and it is not trying to be. It is a floor under the
// guesses that come first. Every rule below rejects something a stranger would try in their
// opening thousand; no rule rejects a password merely for being plain.

// predictablePassword says why a password is a poor one, in the words the form will show, or ""
// when there is nothing to say. personal is whatever the caller knows about the account — the
// address, the person's name, the organisation's — because those are the words an attacker
// starts with rather than arrives at.
func predictablePassword(pw string, personal []string) string {
	folded := strings.ToLower(pw)
	for _, c := range []string{folded, passwordBase(folded)} {
		if c != "" && commonPasswords[c] {
			return "That is one of the first passwords anybody guessing would try. Pick something else."
		}
	}
	if isKeyboardRun(folded) {
		return "That is a straight run of keys, which is guessed about as fast as it is typed. Pick something else."
	}
	if repeatedUnit(folded) {
		return "That is a shorter password written out twice, so it is only as hard to guess as the shorter one. Pick something else."
	}
	if w := builtFromPersonal(folded, personal); w != "" {
		return "That is built around \"" + w + "\", which is written on your account for anyone to read. Pick something that is not."
	}
	return ""
}

// passwordBase reduces a password to the word it was built out of: leetspeak undone and the
// digits and punctuation people decorate with taken off both ends. "P@ssw0rd!23" and "password"
// are the same guess, and a word list that reads literally catches only the second.
//
// Undoing the substitutions has to come after the trim, not before, or the "123" on the end
// turns into "ize" and the word underneath is lost. Both orders are tried anyway, because a
// password starting "@dmin" loses its "@" to the trim and needs the other one.
var decoration = "0123456789 !@#$%^&*()-_=+[]{};:'\",.<>/?\\|`~"

func passwordBase(folded string) string {
	if b := unleet(strings.Trim(folded, decoration)); commonPasswords[b] {
		return b
	}
	return strings.Trim(unleet(folded), decoration)
}

var leet = strings.NewReplacer(
	"@", "a", "4", "a", "8", "b", "3", "e", "6", "g", "9", "g",
	"1", "i", "!", "i", "|", "l", "0", "o", "5", "s", "$", "s",
	"7", "t", "+", "t", "2", "z")

func unleet(s string) string { return leet.Replace(s) }

// walks are the rows and diagonals of a keyboard, plus the digits and the shifted digits. A
// password that is a piece of one of these is a password its owner did not choose so much as
// swipe.
var walks = []string{
	"abcdefghijklmnopqrstuvwxyz",
	"01234567890",
	"!@#$%^&*()",
	"qwertyuiop", "asdfghjkl", "zxcvbnm",
	"qwertzuiop", "azertyuiop", // the German and French layouts type their own runs
	"1qaz2wsx3edc4rfv5tgb6yhn7ujm",
	"qazwsxedcrfvtgbyhnujmikolp",
	"1q2w3e4r5t6y7u8i9o0p",
}

// isKeyboardRun reports whether the whole password is one slide along a row, forwards or back.
// The whole of it, deliberately: "abc" inside a longer password is a coincidence, while a
// password that *is* "abcdefghij" is somewhere in the first hundred guesses.
func isKeyboardRun(folded string) bool {
	if len(folded) < 4 {
		return false
	}
	for _, w := range walks {
		if strings.Contains(w, folded) || strings.Contains(reverseASCII(w), folded) {
			return true
		}
	}
	return false
}

func reverseASCII(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// repeatedUnit reports whether the password is a shorter one typed over and over — "abababab",
// or "password1password1". Eighteen characters that are nine characters twice are nine
// characters to guess, whatever the length counter says. A unit long enough to have passed on
// its own is not repetition worth refusing: two whole words are a passphrase.
func repeatedUnit(folded string) bool {
	n := len(folded)
	for size := 1; size <= n/2 && size < minPasswordBytes; size++ {
		if n%size != 0 {
			continue
		}
		if strings.Repeat(folded[:size], n/size) == folded {
			return true
		}
	}
	return false
}

// builtFromPersonal catches the password made out of the account it is protecting. An address,
// a name and an organisation are the words an attacker has before they begin, and they are
// where a targeted guess starts. An address is reduced to its local part: nobody builds a
// password out of "gmail.com", and refusing every password containing it would be absurd.
func builtFromPersonal(folded string, personal []string) string {
	for _, p := range personal {
		p = strings.ToLower(strings.TrimSpace(p))
		if i := strings.IndexByte(p, '@'); i > 0 {
			p = p[:i]
		}
		for _, word := range strings.FieldsFunc(p, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}) {
			// Four, so that "the" in an organisation's name does not rule out every password
			// with "the" in it, and "acme" does.
			if len(word) >= 4 && strings.Contains(folded, word) {
				return word
			}
		}
	}
	return ""
}

// commonPasswords is the short list rather than the long one: the few hundred words the opening
// guesses are made of, checked after passwordBase has taken the decoration off — which is what
// lets a list this size answer for "password", "Password1", "p@ssw0rd!" and "PASSWORD2024"
// alike. A deployment wanting the ten-million-line breach corpus should hold it outside the
// binary and check it here; this is what fits inside one, and it covers what people reach for
// when a form says "ten characters" and nothing more.
var commonPasswords = wordSet(`
password passwd passe senha passwort

qwerty qwertyuiop qwertz azerty asdfgh asdfghjkl zxcvbn zxcvbnm qazwsx qweasd
qwertyui qwerty123 qazwsxedc

letmein trustno changeme welcome secret access login logon signin default temp
admin administrator adminadmin root toor sysadmin operator manager guest user
username test testing tester demo sample example whatever nothing unknown

iloveyou ilovejesus loveyou lovely lover loveme forever together beautiful
gorgeous sweetie sweetheart darling honey babygirl babyboy angel heaven blessed
princess prince queen king master mistress killer hunter ranger buster gunner
freedom liberty justice victory legend legacy warrior soldier fighter hero

dragon monkey tiger lion eagle falcon phoenix panther cobra viper shark dolphin
kitten kitty puppy doggy bunny pepper ginger peanut cookie muffin cupcake
chocolate vanilla banana orange apple cherry lemon melon mango sugar candy

football baseball basketball hockey soccer softball volleyball tennis golfer
cricket rugby boxing racing nascar cowboys steelers packers yankees dodgers
liverpool arsenal chelsea barcelona madrid juventus manchester united rangers

mustang camaro corvette harley ferrari porsche mercedes maserati bugatti
thunder lightning tornado hurricane blizzard avalanche volcano

computer internet network server database windows linux ubuntu
android iphone samsung google facebook twitter youtube amazon netflix spotify
microsoft oracle nintendo playstation minecraft fortnite pokemon starwars
superman batman spiderman ironman wolverine matrix gandalf frodo hogwarts

michael jennifer jordan jessica ashley joshua matthew daniel andrew robert
thomas william charlie george richard patrick anthony nicholas christopher
michelle nicole hannah maggie amanda melissa samantha elizabeth victoria
sophie olivia emily chloe lauren rachel megan sarah laura anna maria carlos
jackson johnson williams miller davis garcia martinez rodriguez

summer winter spring autumn january february march april june july august
september october november december monday friday weekend holiday birthday
christmas halloween valentine newyear sunshine moonlight starlight rainbow

purple orange yellow silver golden violet indigo crimson scarlet emerald
diamond ruby pearl crystal marble granite

money dollar bank cash rich wealth fortune jackpot casino poker vegas lucky
whiskey vodka tequila brandy cognac guinness corona heineken budweiser

flower butterfly blossom garden forest mountain ocean river valley desert
paradise dreamer dreams believe imagine breathe amazing awesome perfect

hello world helloworld goodbye please thanks sorry maybe never always nothing
something anything everything nobody somebody anybody whatever

shadow phantom ghost demon devil hellfire inferno blaze storm
music guitar piano violin drummer singer dancer artist writer poet
`)

func wordSet(s string) map[string]bool {
	m := make(map[string]bool, 512)
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}
