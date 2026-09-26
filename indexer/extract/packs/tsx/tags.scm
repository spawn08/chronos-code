; Appended to the typescript pack's query (query_from).

; JSX: a capitalised element is a component.
(jsx_opening_element name: (identifier) @name (#match? @name "^[A-Z]")) @ref.instantiate
(jsx_self_closing_element name: (identifier) @name (#match? @name "^[A-Z]")) @ref.instantiate
(jsx_opening_element
  name: (member_expression object: (_) @ref.qualifier property: (property_identifier) @name)) @ref.instantiate
(jsx_self_closing_element
  name: (member_expression object: (_) @ref.qualifier property: (property_identifier) @name)) @ref.instantiate
