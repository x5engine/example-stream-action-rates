package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"google.golang.org/grpc/credentials"
	"io"
	"io/ioutil"
	pbgraphql "main/bp"
	"net/http"
	"os"

	structpb "github.com/golang/protobuf/ptypes/struct"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/oauth"
	"google.golang.org/grpc/grpclog"
)

type Proposal struct {
	Proposer  string `json:"proposer"`
	Name      string `json:"proposal_name"`
	Requested []struct {
		Actor      string `json:"actor"`
		Permission string `json:"permission"`
	} `json:"requested"`
}

type Notification struct {
	DeviceToken string
	Message     string
}

func NewProposal(rawJson string) (*Proposal, error) {
	var p *Proposal

	err := json.Unmarshal([]byte(rawJson), &p)
	if err != nil {
		return nil, fmt.Errorf("unmarshalling: %s", err)
	}

	return p, nil
}

type Server struct {
	apiKey      string
	jwt         *JWT
	oauth2Token *oauth2.Token
	wsConn      *websocket.Conn
	db          *Database
}

func NewServer(apiKey string, db *Database) *Server {
	return &Server{
		apiKey: apiKey,
		db:     db,
	}
}

var logg grpclog.LoggerV2

func init() {
	logg = grpclog.NewLoggerV2(os.Stdout, ioutil.Discard, ioutil.Discard)
	grpclog.SetLoggerV2(logg)
}

func (s *Server) Run(send chan Notification) error {

	cursor := s.db.LoadCursor()

	authToken, err := s.RefreshToken()
	if err != nil {
		return fmt.Errorf("run: %s", err)
	}
	credential := oauth.NewOauthAccess(authToken)
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, "")),
		grpc.WithPerRPCCredentials(credential),
	}

	connection, err := grpc.Dial("mainnet.eos.dfuse.io:443", opts...)
	if err != nil {
		return fmt.Errorf("run: grapheos connection connection: %s", err)
	}

	ctx := context.Background()
	graphqlClient := pbgraphql.NewGraphQLClient(connection)

	queryTemplate := `
		subscription ($search: String!, $cursor: String, $lowBlockNum: Int64) {
		  searchTransactionsForward(query: $search, cursor: $cursor, lowBlockNum: $lowBlockNum) {
			cursor
			undo
			trace {
			  matchingActions {
				receiver
				account
				name
				json
			  }
			}
		  }
		}
`
	search := "account:eosio.msig action:propose"
	vars := toVariable(search, cursor, 0)

	executionClient, err := graphqlClient.Execute(ctx, &pbgraphql.Request{Query: queryTemplate, Variables: vars})
	if err != nil {
		return fmt.Errorf("run: grapheos executionClient: %s", err)
	}

	for {
		response, err := executionClient.Recv()
		if err != nil {
			if err != io.EOF {
				fmt.Println("error receiving message from search stream client:", err)
			}
			fmt.Println("No more result available")
			break
		}
		fmt.Println("Received response:", response.Data)

		//Handling error from lib subscription
		errObjects := gjson.Get(response.Data, "errors").Array()
		if len(errObjects) > 0 {

			for _, e := range errObjects {
				fmt.Println("Error:", gjson.Get(e.Raw, "message"))
			}
			return nil
		}

		cursor := gjson.Get(response.Data, "data.searchTransactionsForward.cursor").Str
		fmt.Println("Cursor:", cursor)
		s.db.StoreCursor(cursor)

		rawProposal := gjson.Get(response.Data, "data.searchTransactionsForward.trace.matchingActions.0.json").Raw
		proposal, err := NewProposal(rawProposal)
		if err != nil {
			return fmt.Errorf("failed to init proposal: %s", err)
		}
		fmt.Println("Proposal name:", proposal.Name)

		undo := gjson.Get(response.Data, "data.searchTransactionsForward.undo").Bool()
		var message string
		if !undo {
			message = fmt.Sprintf("Please approve '%s' proposed by %s", proposal.Name, proposal.Proposer)
		} else {
			message = fmt.Sprintf("Proposal '%s' proposed by %s has been cancel", proposal.Name, proposal.Proposer)
		}

		for _, account := range proposal.Requested {
			deviceToken := s.db.FindDeviceToken(account.Actor)
			if deviceToken != nil {
				fmt.Println("Sending notification to:", account.Actor)
				send <- Notification{
					DeviceToken: deviceToken.Token,
					Message:     message,
				}
			} else {
				fmt.Printf("Actor %s has not opt in for notification\n", account.Actor)
			}
		}
	}

	return nil
}

func (s *Server) RefreshToken() (*oauth2.Token, error) {
	if s.jwt != nil && !s.jwt.NeedRefresh() {
		fmt.Println("Reusing token")
		return s.oauth2Token, nil
	}

	fmt.Println("Getting new token")
	jwt, token, err := s.fetchToken()
	if err != nil {
		return nil, fmt.Errorf("refresh token: %s", err)
	}

	s.jwt = jwt
	s.oauth2Token = &oauth2.Token{
		AccessToken: token,
		TokenType:   "Bearer",
	}

	return s.oauth2Token, nil
}

func (s *Server) fetchToken() (*JWT, string, error) {

	jsonData, err := s.postFetchToken()

	if err != nil {
		return nil, "", fmt.Errorf("http fetch: %s", err)
	}

	var resp *struct {
		Token      string `json:"token"`
		Expiration int64  `json:"expires_at"`
	}

	err = json.Unmarshal(jsonData, &resp)
	if err != nil {
		return nil, "", fmt.Errorf("resp unmarshall: %s", err)
	}

	jwt, err := ParseJwt(resp.Token)
	if err != nil {
		return nil, "", fmt.Errorf("jwt parse: %s", err)
	}

	return jwt, resp.Token, nil
}

func (s *Server) postFetchToken() (body []byte, err error) {

	payload := fmt.Sprintf(`{"api_key":"%s"}`, s.apiKey)

	req, err := http.NewRequest("POST", "https://auth.dfuse.io/v1/auth/issue", bytes.NewBuffer([]byte(payload)))
	if err != nil {
		return nil, fmt.Errorf("request creation: %s", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	httpResp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http post: %s", err)
	}
	defer httpResp.Body.Close()

	fmt.Println("fetch token response Status:", httpResp.Status)

	if httpResp.StatusCode != 200 {
		return nil, fmt.Errorf("http status: %s", httpResp.Status)
	}

	data, err := ioutil.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("response read body: %s", err)
	}
	return data, nil
}

func toVariable(query string, cursor string, lowBlockNum int32) *structpb.Struct {
	return &structpb.Struct{
		Fields: map[string]*structpb.Value{
			"query": {
				Kind: &structpb.Value_StringValue{
					StringValue: query,
				},
			},
			"cursor": {
				Kind: &structpb.Value_StringValue{
					StringValue: cursor,
				},
			},
			"lowBlockNum": {
				Kind: &structpb.Value_NumberValue{
					NumberValue: float64(lowBlockNum),
				},
			},
		},
	}

}
