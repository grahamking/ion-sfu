package server

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	log "github.com/pion/ion-log"
	pb "github.com/pion/ion-sfu/cmd/signal/grpc/proto"
	"github.com/pion/ion-sfu/pkg/sfu"
	"github.com/pion/webrtc/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type SFUServer struct {
	pb.UnimplementedSFUServer
	sync.Mutex
	SFU   *sfu.SFU
	peers map[string]*sfu.Peer
}

func NewServer(s *sfu.SFU) *SFUServer {
	return &SFUServer{
		SFU:   s,
		peers: make(map[string]*sfu.Peer),
	}
}

// Publish a stream to the sfu. Publish creates a bidirectional
// streaming rpc connection between the client and sfu.
//
// The sfu will respond with a message containing the stream pid
// and one of two different payload types:
// 1. `Connect` containing the session answer description. This
// message is *always* returned first.
// 2. `Trickle` containing candidate information for Trickle ICE.
//
// If the webrtc connection is closed, the server will close this stream.
//
// The client should send a message containing the session id
// and one of two different payload types:
// 1. `Connect` containing the session offer description. This
// message must *always* be sent first.
// 2. `Trickle` containing candidate information for Trickle ICE.
//
// If an unidentified client closes this stream, the webrtc stream will be closed.
// Identified clients (sent SignalRequest_Identify) keep running so that they
// can reconnect.
func (s *SFUServer) Signal(stream pb.SFU_SignalServer) error {
	var peer *sfu.Peer
	for {
		in, err := stream.Recv()

		if err != nil {
			if peer != nil && !peer.IsIdentified() {
				// we can only reconnect if the peer identified itself
				peer.Close()
			}

			if err == io.EOF {
				return nil
			}

			errStatus, _ := status.FromError(err)
			if errStatus.Code() == codes.Canceled {
				return nil
			}

			sfu.Logger.Error(fmt.Errorf(errStatus.Message()), "signal error", "code", errStatus.Code())
			return err
		}

		switch payload := in.Payload.(type) {
		case *pb.SignalRequest_Identify:
			ident := payload.Identify
			if peer == nil {
				// not joined yet, try to restore it
				if p, ok := s.peers[ident]; ok {
					peer = p
					log.Debugf("restored peer %s", ident)
				}
			} else {
				peer.SetIdentity(ident)
				s.peers[ident] = peer
				log.Debugf("identified peer %s", ident)
			}

		case *pb.SignalRequest_Join:
			sfu.Logger.V(1).Info("signal->join", "called", string(payload.Join.Description))
			if peer == nil {
				peer = sfu.NewPeer(s.SFU)
			}

			var offer webrtc.SessionDescription
			err := json.Unmarshal(payload.Join.Description, &offer)
			if err != nil {
				s.Lock()
				err = stream.Send(&pb.SignalReply{
					Payload: &pb.SignalReply_Error{
						Error: fmt.Errorf("join sdp unmarshal error: %w", err).Error(),
					},
				})
				s.Unlock()
				if err != nil {
					sfu.Logger.Error(err, "grpc send error")
					return status.Errorf(codes.Internal, err.Error())
				}
			}

			// Notify user of new ice candidate
			peer.OnIceCandidate = func(candidate *webrtc.ICECandidateInit, target int) {
				bytes, err := json.Marshal(candidate)
				if err != nil {
					sfu.Logger.Error(err, "OnIceCandidate error")
				}
				s.Lock()
				err = stream.Send(&pb.SignalReply{
					Payload: &pb.SignalReply_Trickle{
						Trickle: &pb.Trickle{
							Init:   string(bytes),
							Target: pb.Trickle_Target(target),
						},
					},
				})
				s.Unlock()
				if err != nil {
					sfu.Logger.Error(err, "OnIceCandidate send error")
				}
			}

			// Notify user of new offer
			peer.OnOffer = func(o *webrtc.SessionDescription) {
				marshalled, err := json.Marshal(o)
				if err != nil {
					s.Lock()
					err = stream.Send(&pb.SignalReply{
						Payload: &pb.SignalReply_Error{
							Error: fmt.Errorf("offer sdp marshal error: %w", err).Error(),
						},
					})
					s.Unlock()
					if err != nil {
						sfu.Logger.Error(err, "grpc send error")
					}
					return
				}

				s.Lock()
				err = stream.Send(&pb.SignalReply{
					Payload: &pb.SignalReply_Description{
						Description: marshalled,
					},
				})
				s.Unlock()

				if err != nil {
					sfu.Logger.Error(err, "negotiation error")
				}
			}

			peer.OnICEConnectionStateChange = func(c webrtc.ICEConnectionState) {
				s.Lock()
				err = stream.Send(&pb.SignalReply{
					Payload: &pb.SignalReply_IceConnectionState{
						IceConnectionState: c.String(),
					},
				})
				s.Unlock()

				if err != nil {
					sfu.Logger.Error(err, "oniceconnectionstatechange error")
				}

				if c == webrtc.ICEConnectionStateClosed {
					if peer != nil && peer.IsIdentified() {
						peer.Close()
						delete(s.peers, peer.GetIdentity())
					}
				}
			}

			err = peer.Join(payload.Join.Sid, payload.Join.Uid)
			if err != nil {
				switch err {
				case sfu.ErrTransportExists:
					fallthrough
				case sfu.ErrOfferIgnored:
					s.Lock()
					err = stream.Send(&pb.SignalReply{
						Payload: &pb.SignalReply_Error{
							Error: fmt.Errorf("join error: %w", err).Error(),
						},
					})
					s.Unlock()
					if err != nil {
						sfu.Logger.Error(err, "grpc send error")
						return status.Errorf(codes.Internal, err.Error())
					}
				default:
					return status.Errorf(codes.Unknown, err.Error())
				}
			}

			answer, err := peer.Answer(offer)
			if err != nil {
				return status.Errorf(codes.Internal, fmt.Sprintf("answer error: %v", err))
			}

			marshalled, err := json.Marshal(answer)
			if err != nil {
				return status.Errorf(codes.Internal, fmt.Sprintf("sdp marshal error: %v", err))
			}

			// send answer
			s.Lock()
			err = stream.Send(&pb.SignalReply{
				Id: in.Id,
				Payload: &pb.SignalReply_Join{
					Join: &pb.JoinReply{
						Description: marshalled,
					},
				},
			})
			s.Unlock()

			if err != nil {
				sfu.Logger.Error(err, "error sending join response")
				return status.Errorf(codes.Internal, "join error %s", err)
			}

		case *pb.SignalRequest_Description:
			if peer == nil {
				peer = sfu.NewPeer(s.SFU)
			}
			var sdp webrtc.SessionDescription
			err := json.Unmarshal(payload.Description, &sdp)
			if err != nil {
				s.Lock()
				err = stream.Send(&pb.SignalReply{
					Payload: &pb.SignalReply_Error{
						Error: fmt.Errorf("negotiate sdp unmarshal error: %w", err).Error(),
					},
				})
				s.Unlock()
				if err != nil {
					sfu.Logger.Error(err, "grpc send error")
					return status.Errorf(codes.Internal, err.Error())
				}
			}

			if sdp.Type == webrtc.SDPTypeOffer {
				answer, err := peer.Answer(sdp)
				if err != nil {
					switch err {
					case sfu.ErrNoTransportEstablished:
						fallthrough
					case sfu.ErrOfferIgnored:
						s.Lock()
						err = stream.Send(&pb.SignalReply{
							Payload: &pb.SignalReply_Error{
								Error: fmt.Errorf("negotiate answer error: %w", err).Error(),
							},
						})
						s.Unlock()
						if err != nil {
							sfu.Logger.Error(err, "grpc send error")
							return status.Errorf(codes.Internal, err.Error())
						}
						continue
					default:
						return status.Errorf(codes.Unknown, fmt.Sprintf("negotiate error: %v", err))
					}
				}

				marshalled, err := json.Marshal(answer)
				if err != nil {
					s.Lock()
					err = stream.Send(&pb.SignalReply{
						Payload: &pb.SignalReply_Error{
							Error: fmt.Errorf("sdp marshal error: %w", err).Error(),
						},
					})
					s.Unlock()
					if err != nil {
						sfu.Logger.Error(err, "grpc send error")
						return status.Errorf(codes.Internal, err.Error())
					}
				}

				s.Lock()
				err = stream.Send(&pb.SignalReply{
					Id: in.Id,
					Payload: &pb.SignalReply_Description{
						Description: marshalled,
					},
				})
				s.Unlock()

				if err != nil {
					return status.Errorf(codes.Internal, fmt.Sprintf("negotiate error: %v", err))
				}

			} else if sdp.Type == webrtc.SDPTypeAnswer {
				err := peer.SetRemoteDescription(sdp)
				if err != nil {
					switch err {
					case sfu.ErrNoTransportEstablished:
						s.Lock()
						err = stream.Send(&pb.SignalReply{
							Payload: &pb.SignalReply_Error{
								Error: fmt.Errorf("set remote description error: %w", err).Error(),
							},
						})
						s.Unlock()
						if err != nil {
							sfu.Logger.Error(err, "grpc send error")
							return status.Errorf(codes.Internal, err.Error())
						}
					default:
						return status.Errorf(codes.Unknown, err.Error())
					}
				}
			}

		case *pb.SignalRequest_Trickle:
			if peer == nil {
				return status.Error(
					codes.FailedPrecondition,
					"Received Trickle before Join",
				)
			}
			var candidate webrtc.ICECandidateInit
			err := json.Unmarshal([]byte(payload.Trickle.Init), &candidate)
			if err != nil {
				sfu.Logger.Error(err, "error parsing ice candidate")
				s.Lock()
				err = stream.Send(&pb.SignalReply{
					Payload: &pb.SignalReply_Error{
						Error: fmt.Errorf("unmarshal ice candidate error:  %w", err).Error(),
					},
				})
				s.Unlock()
				if err != nil {
					sfu.Logger.Error(err, "grpc send error ")
					return status.Errorf(codes.Internal, err.Error())
				}
				continue
			}

			err = peer.Trickle(candidate, int(payload.Trickle.Target))
			if err != nil {
				switch err {
				case sfu.ErrNoTransportEstablished:
					sfu.Logger.Error(err, "peer hasn't joined")
					s.Lock()
					err = stream.Send(&pb.SignalReply{
						Payload: &pb.SignalReply_Error{
							Error: fmt.Errorf("trickle error:  %w", err).Error(),
						},
					})
					s.Unlock()
					if err != nil {
						sfu.Logger.Error(err, "grpc send error")
						return status.Errorf(codes.Internal, err.Error())
					}
				default:
					return status.Errorf(codes.Unknown, fmt.Sprintf("negotiate error: %v", err))
				}
			}

		}
	}
}
