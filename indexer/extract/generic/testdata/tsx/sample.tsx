import React, { useState } from "react";
import { Button } from "./button";

interface Props {
  title: string;
}

/** Counter shows a count. */
export function Counter({ title }: Props) {
  const [n, setN] = useState(0);
  return <Button onClick={() => setN(n + 1)}>{title}</Button>;
}

export const Panel: React.FC<Props> = (props) => {
  return <div>{format(props.title)}</div>;
};

export default class App extends React.Component<Props> {
  render() {
    return <Counter title="x" />;
  }
}
