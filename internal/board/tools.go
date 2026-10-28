package board

import "google.golang.org/genai"

// The tool descriptions are the model's whole manual for the board, so
// they say when to call each tool, not only what it does. Changing them
// changes how the model draws: check a live session after editing.

func str(desc string) *genai.Schema { return &genai.Schema{Type: genai.TypeString, Description: desc} }

func strs(desc string) *genai.Schema {
	return &genai.Schema{Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString}, Description: desc}
}

func kindSchema() *genai.Schema {
	enum := make([]string, len(Kinds))
	for i, k := range Kinds {
		enum[i] = string(k)
	}
	return &genai.Schema{Type: genai.TypeString, Enum: enum, Description: "What the box stands for. " +
		"idea: a thing, a concept or a point. step: one step in an order: a how-to, a plan, next steps. " +
		"option: one of the choices being weighed. person: someone involved. " +
		"fact: a number, a date, a name or a definition worth keeping in view. " +
		"question: something still open, to ask them or to find out; label it with the question, like \"Which weekend?\". " +
		"example: a concrete example (give on to hang it under the box it's an example of). " +
		"pro: an upside of the box it's on (needs on). con: a downside or a risk of the box it's on (needs on)."}
}

func object(props map[string]*genai.Schema, required ...string) *genai.Schema {
	return &genai.Schema{Type: genai.TypeObject, Properties: props, Required: required}
}

// Tools declares the board to the model. Drawing tools are non-blocking so
// the model keeps talking while the board changes; get_board blocks
// because the model needs its answer.
func Tools() []*genai.Tool {
	nb := genai.BehaviorNonBlocking
	return []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{
		{
			Name: AddNode,
			Description: "Put a box on the board, or change one. Call it the moment something worth keeping in view comes up, " +
				"while you talk about it: one box per call, as you go, not a batch at the end. " +
				"Pick a short snake_case id (train, book_seats) and reuse it to change that box later.",
			Behavior: nb,
			Parameters: object(map[string]*genai.Schema{
				"id":    str("Short snake_case id you choose, unique on the board."),
				"kind":  kindSchema(),
				"label": str("2 to 5 words, in the person's own words."),
				"after": str("Id of the box this one follows: the step before it, or what leads to it. Draws the arrow from it. " +
					"Leave out when nothing leads to it: the first box, or choices side by side (they never follow each other)."),
				"on":     str("For pro, con and example: the id of the box it's about. It's drawn under that box."),
				"group":  str("Id of a group (made with group) to put the box in."),
				"detail": str("One short line under the label: a number, a date, a short qualifier."),
			}, "id", "kind", "label"),
		},
		{
			Name:        Connect,
			Description: "Draw an arrow between two boxes, when add_node's after didn't already: a second path, a loop back, a cause and what it leads to.",
			Behavior:    nb,
			Parameters: object(map[string]*genai.Schema{
				"from":  str("Id of the box the arrow leaves."),
				"to":    str("Id of the box it points at."),
				"label": str("A word or two on the arrow (\"if urgent\", \"weekly\"). Optional."),
			}, "from", "to"),
		},
		{
			Name:        Frame,
			Description: "Draw a labeled frame around boxes that belong together: a phase, a category, one side of a comparison.",
			Behavior:    nb,
			Parameters: object(map[string]*genai.Schema{
				"id":      str("Short snake_case id for the group."),
				"label":   str("2 to 4 words."),
				"members": strs("Ids of the boxes inside it."),
			}, "id", "label", "members"),
		},
		{
			Name:        Annotate,
			Description: "Write a short line under a box: a number, a source, a caveat. Empty text removes the note.",
			Behavior:    nb,
			Parameters: object(map[string]*genai.Schema{
				"target": str("Id of the box."),
				"text":   str("A few words."),
			}, "target", "text"),
		},
		{
			Name:        Rename,
			Description: "Change the label of a box or a group, when the person corrects you or finds a better word.",
			Behavior:    nb,
			Parameters: object(map[string]*genai.Schema{
				"id":    str("Id of the box or group."),
				"label": str("The new label."),
			}, "id", "label"),
		},
		{
			Name:        Remove,
			Description: "Take boxes, groups or arrows off the board, when the person says it's wrong or not part of it. Name an arrow from->to.",
			Behavior:    nb,
			Parameters: object(map[string]*genai.Schema{
				"ids": strs("Ids to remove; arrows as from->to."),
			}, "ids"),
		},
		{
			Name: PointAt,
			Description: "Highlight boxes on the board while you talk about them, like pointing at a whiteboard: " +
				"\"so this one here…\". Call it right as you start talking about something already on the board.",
			Behavior: nb,
			Parameters: object(map[string]*genai.Schema{
				"ids": strs("Ids of the boxes or groups to highlight."),
				"ms":  {Type: genai.TypeInteger, Description: "How long to highlight, in milliseconds. Default 1800."},
			}, "ids"),
		},
		{
			Name:        GetBoard,
			Description: "Read what's on the board right now, as text. Use it when you're not sure what's there.",
			Behavior:    genai.BehaviorBlocking,
		},
	}}}
}

// Answer applies a tool call to doc and returns the op and the response to
// send back. The model sends its drawing calls before it speaks and only
// speaks once they're answered, so every answer is scheduled for when the
// model is idle: a silent answer would leave it mute. A rejected call gets
// the reason, so the model can fix it; get_board gets the digest.
func Answer(doc *Doc, c *genai.FunctionCall) (Op, *genai.FunctionResponse, error) {
	r := &genai.FunctionResponse{ID: c.ID, Name: c.Name, Scheduling: genai.FunctionResponseSchedulingWhenIdle}
	op, err := Decode(c.Name, c.Args)
	if err == nil {
		err = doc.Apply(op)
	}
	switch {
	case err != nil:
		r.Response = map[string]any{"error": err.Error()}
	case op.Name == GetBoard:
		r.Response = map[string]any{"board": doc.Digest()}
	default:
		r.Response = map[string]any{"ok": true}
	}
	return op, r, err
}
