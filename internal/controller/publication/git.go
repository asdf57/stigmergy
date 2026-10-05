package publication

import (
	"github.com/asdf57/stigmergy/internal/gitpublication"
)

// Compatibility aliases keep callers stable while sharing the Git transport.
type Publisher = gitpublication.Publisher
type GitPublisher = gitpublication.GitPublisher
type PublishRequest = gitpublication.PublishRequest
type PublishResult = gitpublication.PublishResult
type Artifact = gitpublication.Artifact

var NewGitPublisher = gitpublication.NewGitPublisher
var safePublicationRoot = gitpublication.SafeRoot
var safeRepositoryPath = gitpublication.SafePath
var validateArtifacts = gitpublication.ValidateArtifacts
