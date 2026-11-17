package store

func validateWorkerArtifactInput(i WorkerArtifactInput, key string) error {
	if !workspaceOperationText(key, 256, false) || (i.OperationID == "") == (i.RelativePath == "") ||
		!workspaceOperationText(i.OperationID, 256, true) || i.RelativePath != "" && !workspaceOperationPath(i.RelativePath) ||
		!workspaceOperationText(i.ArtifactID, 256, true) || !workspaceOperationText(i.ExpectedVersionID, 256, true) ||
		!workspaceOperationText(i.Name, 1024, i.ArtifactID != "") ||
		!(i.ArtifactID == "" && i.ExpectedVersionID == "" || i.ArtifactID != "" && i.Name == "" && i.ExpectedVersionID != "") {
		return ErrInvalid
	}
	return nil
}

func validateWorkerArtifactBlob(blob WorkerArtifactBlob) error {
	if !workspaceOperationHash(blob.Hash) || !workspaceOperationSize(blob.Size) {
		return ErrInvalid
	}
	return nil
}
