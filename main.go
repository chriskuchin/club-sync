package main

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
	strava "github.com/strava/go.strava"
	"github.com/urfave/cli/v2"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/sheets/v4"
)

type (
	// Token OAuth Token Struct
	Token struct {
		TokenType    string `json:"token_type"`
		ExpiresAt    Time   `json:"expires_at"`
		RefreshToken string `json:"refresh_token"`
		AccessToken  string `json:"access_token"`
	}

	Member struct {
		FirstName string
		LastName  string
	}

	Activity struct {
		Athlete            Member  `json:"athlete"`
		Name               string  `json:"name"`
		Distance           float64 `json:"distance"`
		MovingTime         int     `json:"moving_time"`
		ElapsedTime        int     `json:"elapsed_time"`
		TotalElevationGain float64 `json:"total_elevation_gain"`
		Type               string  `json:"type"`
	}

	Trigger struct {
		Token     *Token
		QueryFrom time.Time
	}

	// Time type to enable handling of unix timstamps better
	Time time.Time
)

var (
	members               = []int{16118411}
	clientID              int
	clientSecret          string
	clubID                int
	backfillActivities    int
	googleOauthConfig     *oauth2.Config
	googleTokenPath       string
	stravaTokenPath       string
	resumeTokenPath       string
	googleCredentialsPath string
	initialResumeToken    string

	contestStart = time.Date(2020, time.August, 12, 11, 59, 0, 0, time.UTC)
	port         = 8080

	authenticator *strava.OAuthAuthenticator
	callbackURL   = fmt.Sprintf("http://localhost:%d/strava/exchange_token", port)

	interval time.Duration
)

func main() {
	app := &cli.App{
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:        "client-id",
				Usage:       "Strava API Client ID",
				EnvVars:     []string{"STRAVA_CLIENT_ID"},
				Destination: &clientID,
			},
			&cli.StringFlag{
				Name:        "client-secret",
				Usage:       "Strava API Client Secret",
				EnvVars:     []string{"STRAVA_CLIENT_SECRET"},
				Destination: &clientSecret,
			},
			&cli.IntFlag{
				Name:        "club-id",
				Usage:       "Strava Club ID to Sync & Monitor",
				EnvVars:     []string{"STRAVA_CLUB_ID"},
				Destination: &clubID,
			},
			&cli.IntFlag{
				Name:        "backfill",
				Usage:       "How man events to backfill",
				EnvVars:     []string{"ACTIVITY_BACKFILL"},
				Destination: &backfillActivities,
			},
			&cli.StringFlag{
				Name:        "google-token-path",
				Usage:       "The path to save the google token or look for the google token",
				Value:       "google.json",
				EnvVars:     []string{"GOOGLE_TOKEN_PATH"},
				Destination: &googleTokenPath,
			},
			&cli.StringFlag{
				Name:        "strava-token-path",
				Usage:       "The path to save the google token or look for the strava token",
				Value:       "token.json",
				EnvVars:     []string{"STRAVA_TOKEN_PATH"},
				Destination: &stravaTokenPath,
			},
			&cli.StringFlag{
				Name:        "resume-token-path",
				Usage:       "The path to save the google token or look for the strava token",
				Value:       "resume.json",
				EnvVars:     []string{"RESUME_TOKEN_PATH"},
				Destination: &resumeTokenPath,
			},
			&cli.StringFlag{
				Name:        "credentials-file",
				Usage:       "The path to save the google token or look for the strava token",
				Value:       "credentials.json",
				EnvVars:     []string{"GOOGLE_CREDENTIALS_FILE"},
				Destination: &googleCredentialsPath,
			},
			&cli.StringFlag{
				Name:        "resume",
				Usage:       "The path to save the google token or look for the strava token",
				EnvVars:     []string{"RESUME_TOKEN"},
				Destination: &initialResumeToken,
			},
			&cli.DurationFlag{
				Name:        "interval",
				Usage:       "The path to save the google token or look for the strava token",
				Value:       1 * time.Hour,
				EnvVars:     []string{"INTERVAL"},
				Destination: &interval,
			},
		},
		Action: func(c *cli.Context) error {
			if initialResumeToken != "" {
				saveRecentActivityID(resumeTokenPath, initialResumeToken)
			}

			b, err := ioutil.ReadFile(googleCredentialsPath)
			if err != nil {
				logrus.Errorf("Unable to read client secret file: %v", err)
				return err
			}

			googleOauthConfig, err = google.ConfigFromJSON(b, sheets.SpreadsheetsScope)
			if err != nil {
				logrus.Infof("Unable to parse client secret file to config: %v", err)
			}

			http.HandleFunc("/strava/auth", stravaRedirectHandler)
			http.HandleFunc("/strava/exchange_token", stravaExchangeTokenHandler)
			http.HandleFunc("/google/auth", googleRedirectHandler)
			http.HandleFunc("/google/exchange_token", googleExchangeTokenHandler)

			activityPollTrigger := make(chan *Trigger)
			go processActivities(activityPollTrigger)
			go pollClubActivities(activityPollTrigger)

			http.ListenAndServe(fmt.Sprintf(":%d", port), nil)

			return nil
		},
	}

	app.Run(os.Args)
}

func processActivities(triggers chan *Trigger) {
	for {
		sleep := time.Now().Truncate(interval).Add(interval).Sub(time.Now())
		time.Sleep(sleep)

		token, err := tokenFromFile(stravaTokenPath)
		if err != nil {
			logrus.Errorf("Failed to retrieve token: %+v", err)
			continue
		}

		token = refreshToken(token)
		if token == nil {
			logrus.Warn("Please authorize the app by visiting http://localhost:8080/strava/auth")
			continue
		}

		triggers <- &Trigger{
			Token:     token,
			QueryFrom: time.Now().Add(-interval).Truncate(interval),
		}
	}
}

func pollClubActivities(triggers chan *Trigger) {
	for trigger := range triggers {
		lastActivity, err := getLastActivityID(resumeTokenPath)
		if err != nil {
			logrus.Warnf("First run: %+v", err)
		}

		googTok, err := googleTokenFromFile(googleTokenPath)
		if err != nil {
			logrus.Errorf("Failed to retrieve google token: %+v", err)
		}

		svc, err := sheets.New(googleOauthConfig.Client(context.Background(), googTok))
		if err != nil {
			logrus.Errorf("Failed to instantiate sheets service: %+v", err)
		}

		page := 1
		batchSize := 50
		firstActivity := ""
		activities := getClubActivities(trigger.Token.AccessToken, clubID, batchSize, page)
		if activities == nil {
			logrus.Error("Failed to retrieve club activities")
			break
		}

		processedActivitiesCount := 0
		rows := [][]interface{}{}
		for _, activity := range activities {
			row := []interface{}{}
			if backfillActivities != 0 && lastActivity == "" && processedActivitiesCount == backfillActivities {
				break
			}
			if firstActivity == "" {
				firstActivity = activity.getActivityHash()
				saveRecentActivityID(resumeTokenPath, firstActivity)
			}

			if lastActivity != "" && lastActivity == activity.getActivityHash() {
				logrus.Infof("Found the previous last item processed: %s", activity.getActivityHash())
				break
			}

			row = append(row, time.Now().Format("January 2, 2006"))
			row = append(row, fmt.Sprintf("%.2f", activity.getDistanceInMiles()))
			row = append(row, fmt.Sprintf("%.2f", activity.TotalElevationGain))
			row = append(row, printDuration(activity.getMovingTimeDuration()))
			row = append(row, activity.Type)
			row = append(row, fmt.Sprintf("%s %s", activity.Athlete.FirstName, activity.Athlete.LastName))

			rows = append(rows, row)
			processedActivitiesCount++
		}

		if svc != nil {
			res, err := svc.Spreadsheets.Values.Append("1hYZGyFuEAuv61SUkRaCpFEz1LZWGxC1DkCxWTEcG8JE", "Sheet1!A:A", &sheets.ValueRange{
				Values:         rows,
				MajorDimension: "ROWS",
			}).ValueInputOption("USER_ENTERED").Do()
			if err != nil {
				logrus.Errorf("Failed to write to sheet: %+v", err)
			}

			logrus.Info(res)
		}
	}
}

func googleRedirectHandler(w http.ResponseWriter, r *http.Request) {
	url := googleOauthConfig.AuthCodeURL("state", oauth2.AccessTypeOffline)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func googleExchangeTokenHandler(w http.ResponseWriter, r *http.Request) {
	tok, err := googleOauthConfig.Exchange(context.TODO(), r.FormValue("code"))
	if err != nil {
		logrus.Errorf("Unable to retrieve token from web: %v", err)
		return
	}

	saveGoogleToken(googleTokenPath, tok)
}

func stravaRedirectHandler(w http.ResponseWriter, r *http.Request) {
	url := fmt.Sprintf("https://www.strava.com/oauth/authorize?client_id=%d&redirect_uri=%s&response_type=code&scope=read_all", clientID, callbackURL)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func stravaExchangeTokenHandler(w http.ResponseWriter, r *http.Request) {
	result, err := http.Post(fmt.Sprintf("https://www.strava.com/oauth/token?client_id=%d&client_secret=%s&code=%s&grant_type=authorization_code", clientID, clientSecret, r.FormValue("code")), "application/json", nil)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("%#v", err)))
	}

	contents, err := ioutil.ReadAll(result.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(fmt.Sprintf("%#v", err)))
	}

	token, err := handleTokenResponse(contents)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	saveToken(stravaTokenPath, token)
}

func refreshToken(token *Token) *Token {
	if token != nil && time.Now().After(time.Time(token.ExpiresAt)) {
		result, err := http.Post(fmt.Sprintf("https://www.strava.com/oauth/token?client_id=%d&client_secret=%s&refresh_token=%s&grant_type=refresh_token", clientID, clientSecret, token.RefreshToken), "application/json", nil)
		if err != nil {
			logrus.Errorf("Failed to refresh access token: %v", err)
			return nil
		}

		contents, err := ioutil.ReadAll(result.Body)
		if err != nil {
			logrus.Errorf("Failed to read refresh token: %v", err)
			return nil
		}

		token, err := handleTokenResponse(contents)
		if err != nil {
			logrus.Errorf("Failed to process and handle refresh token: %+v", err)
			return nil
		}

		saveToken(stravaTokenPath, token)

		return token
	} else if token == nil {
		logrus.Info("Application has not been authorized yet please visit http://localhost:8080/strava/auth")
		return nil
	}

	logrus.Infof("Token does not need refreshing expires at: %v", time.Time(token.ExpiresAt))
	return token
}

func saveToken(path string, token *Token) {
	fmt.Printf("Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		logrus.Errorf("Unable to cache oauth token: %v", err)
		return
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

func saveGoogleToken(path string, token *oauth2.Token) {
	fmt.Printf("Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		logrus.Errorf("Unable to cache oauth token: %v", err)
		return
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

func saveRecentActivityID(path string, activityID string) {
	fmt.Printf("Saving activity id file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		logrus.Errorf("Unable to cache oauth token: %v", err)
		return
	}
	defer f.Close()
	json.NewEncoder(f).Encode(activityID)
}

func tokenFromFile(file string) (*Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func googleTokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func getLastActivityID(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	id := ""
	err = json.NewDecoder(f).Decode(&id)
	return id, err
}

func handleTokenResponse(contents []byte) (*Token, error) {
	token := &Token{}
	err := json.Unmarshal(contents, token)
	if err != nil {
		logrus.Errorf("Failed to process token: %+v", err)
		return nil, err
	}

	return token, nil
}

// MarshalJSON is used to convert the timestamp to JSON
func (t Time) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(time.Time(t).Unix(), 10)), nil
}

// UnmarshalJSON is used to convert the timestamp from JSON
func (t *Time) UnmarshalJSON(s []byte) (err error) {
	r := string(s)
	q, err := strconv.ParseInt(r, 10, 64)
	if err != nil {
		return err
	}
	*(*time.Time)(t) = time.Unix(q, 0)
	return nil
}

func getClubActivities(token string, clubID, batchSize, page int) []Activity {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://www.strava.com/api/v3/clubs/%d/activities?page=%d&per_page=%d", clubID, page, batchSize), nil)
	if err != nil {
		logrus.WithField("club-id", clubID).WithField("batch-size", batchSize).WithField("page", page).Errorf("Failed to create request for club activities: %+v", err)
		return nil
	}
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %s", token))

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		logrus.Errorf("Failed to make request: %+v", err)
		return nil
	}

	contents, err := ioutil.ReadAll(res.Body)
	if err != nil {
		logrus.Errorf("Failed to read response: %+v", err)
		return nil
	}

	logrus.Infof("%s", contents)

	activities := []Activity{}
	err = json.Unmarshal(contents, &activities)
	if err != nil {
		logrus.Errorf("Failed to unserialize list of activities: %+v", err)
		return nil
	}

	return activities
}

func (a *Activity) getActivityHash() string {
	// firstName+ lastname+distance+name
	activityID := fmt.Sprintf("%s%s%f%d", a.Athlete.FirstName, a.Athlete.LastName, a.Distance, a.ElapsedTime)
	h := sha1.New()
	h.Write([]byte(activityID))

	return fmt.Sprintf("%x", h.Sum(nil))
}

func (a *Activity) getDistanceInMiles() float64 {
	return a.Distance * 0.000621371
}

func (a *Activity) getElapsedTimeDuration() time.Duration {
	return time.Duration(a.ElapsedTime) * time.Second
}

func (a *Activity) getMovingTimeDuration() time.Duration {
	return time.Duration(a.MovingTime) * time.Second
}

func printDuration(dur time.Duration) string {
	hour := dur / time.Hour
	dur -= hour * time.Hour
	minute := dur / time.Minute
	dur -= minute * time.Minute
	second := dur / time.Second

	if dur > time.Hour {
		return fmt.Sprintf("%02d:%02d:%02d", hour, minute, second)
	}

	return fmt.Sprintf("%02d:%02d:00", minute, second)
}
