package redact

// HiddenMarker is the one literal that stands for an issue the reader cannot
// see: the title of the issue tree's stub and the replacement of every
// identifier of an unavailable issue in event data. It carries no number,
// prefix, project or index, and is the same whatever made the issue
// unavailable.
const HiddenMarker = "<hidden>"
